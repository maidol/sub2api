package vpngate

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNoHealthyNode means no node was healthy, free and out of cooldown.
var ErrNoHealthyNode = errors.New("no healthy free node")

// ManagerOptions tunes health checking and reselection.
type ManagerOptions struct {
	ProbeURL      string
	ProbeTimeout  time.Duration
	FailThreshold int           // consecutive probe failures before a slot moves
	Cooldown      time.Duration // a failed node is not picked again for this long
	MaxAttempts   int           // candidates probed per reselection
	// Active returns the slots that are in use (leased). Only they are
	// health-checked, and only their nodes count as taken. nil means all slots.
	Active  func() []Slot
	Now     func() time.Time
	Shuffle func([]string)
	Logf    func(format string, args ...any)
}

// Manager keeps every active slot on a healthy node. A slot keeps its node for
// as long as the node passes probes; only after FailThreshold consecutive
// failures (or when two slots share a node) does it move to a random node
// that is healthy, not in cooldown, and not used by any other active slot.
// Reselect moves a slot on demand (new lease, "rotate").
//
// All methods are safe for concurrent use. Manager state transitions serialize
// on one mutex; Tick probes and CurrentNode may run concurrently with Reselect.
type Manager struct {
	mu         sync.Mutex
	ctrl       Controller
	slots      []Slot
	nodes      []string
	opts       ManagerOptions
	fails      map[string]int       // slot name -> consecutive failures of its node
	badUntil   map[string]time.Time // node name -> end of cooldown
	generation uint64               // invalidates probes started before a pause/reload
}

func NewManager(ctrl Controller, slots []Slot, nodes []Node, opts ManagerOptions) *Manager {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	if opts.FailThreshold < 1 {
		opts.FailThreshold = 1
	}
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = 1
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Shuffle == nil {
		opts.Shuffle = CryptoShuffle
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &Manager{
		ctrl:     ctrl,
		slots:    slots,
		nodes:    names,
		opts:     opts,
		fails:    map[string]int{},
		badUntil: map[string]time.Time{},
	}
}

func (m *Manager) active() []Slot {
	if m.opts.Active == nil {
		return m.slots
	}
	return m.opts.Active()
}

// Pause blocks Tick, Reselect and PinCurrent until resume is called. CurrentNode
// reads the controller directly and remains available. A pause invalidates any
// Tick probes already in flight. resume replaces the candidate nodes when nodes
// is non-nil. Failure counts and cooldowns are kept across pool reloads.
func (m *Manager) Pause() (resume func(nodes []Node)) {
	m.mu.Lock()
	m.generation++
	var once sync.Once
	return func(nodes []Node) {
		once.Do(func() {
			if nodes != nil {
				names := make([]string, 0, len(nodes))
				for _, n := range nodes {
					names = append(names, n.Name)
				}
				m.nodes = names
			}
			m.mu.Unlock()
		})
	}
}

// Run pins every slot's starting node, calls Tick immediately and then every
// interval until ctx is done.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	m.PinCurrent(ctx)
	m.Tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Tick(ctx)
		}
	}
}

// PinCurrent selects every slot's current node again through the API. Mihomo
// persists only selections made through the API (profile.store-selected); a
// group's initial node comes from the shuffled config, which is re-rolled on
// every start, so without this a restart moves every slot that never failed.
func (m *Manager) PinCurrent(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.slots {
		node, err := m.ctrl.Current(ctx, s.Group())
		if err != nil {
			m.opts.Logf("vpngate: %s: read selection: %v", s.Name, err)
			continue
		}
		if err := m.ctrl.Select(ctx, s.Group(), node); err != nil {
			m.opts.Logf("vpngate: %s: pin %q: %v", s.Name, node, err)
		}
	}
}

// CurrentNode returns the node a slot currently uses.
func (m *Manager) CurrentNode(ctx context.Context, s Slot) (string, error) {
	return m.ctrl.Current(ctx, s.Group())
}

// Tick checks every active slot once. Network probes run without holding m.mu;
// their results are discarded after a pause or if the controller changed nodes.
func (m *Manager) Tick(ctx context.Context) {
	slots := m.active()
	m.mu.Lock()
	now := m.opts.Now()
	current := make(map[string]string, len(slots))
	for _, s := range slots {
		node, err := m.ctrl.Current(ctx, s.Group())
		if err != nil {
			m.opts.Logf("vpngate: %s: read selection: %v", s.Name, err)
			continue
		}
		current[s.Name] = node
	}
	generation := m.generation
	m.mu.Unlock()

	claimed := map[string]string{}
	for _, s := range slots {
		cur, ok := current[s.Name]
		if !ok {
			continue
		}
		m.mu.Lock()
		if m.generation != generation {
			m.mu.Unlock()
			return
		}
		live, err := m.ctrl.Current(ctx, s.Group())
		if err != nil || live != cur {
			m.mu.Unlock()
			continue
		}
		if owner, dup := claimed[cur]; dup {
			m.opts.Logf("vpngate: %s: node %q is already used by %s, reselecting", s.Name, cur, owner)
			m.mu.Unlock()
			m.tickReselect(ctx, s, cur, slots, claimed, now, generation)
			continue
		}
		m.mu.Unlock()

		probeErr := m.ctrl.Delay(ctx, cur, m.opts.ProbeURL, m.opts.ProbeTimeout)
		m.mu.Lock()
		if m.generation != generation {
			m.mu.Unlock()
			return
		}
		if ctx.Err() != nil {
			m.mu.Unlock()
			continue
		}
		live, err = m.ctrl.Current(ctx, s.Group())
		if err != nil || live != cur {
			m.mu.Unlock()
			continue
		}
		if probeErr != nil {
			m.fails[s.Name]++
			m.opts.Logf("vpngate: %s: node %q probe failed (%d/%d): %v",
				s.Name, cur, m.fails[s.Name], m.opts.FailThreshold, probeErr)
			if m.fails[s.Name] < m.opts.FailThreshold {
				claimed[cur] = s.Name
				m.mu.Unlock()
				continue
			}
			m.badUntil[cur] = now.Add(m.opts.Cooldown)
			m.mu.Unlock()
			m.tickReselect(ctx, s, cur, slots, claimed, now, generation)
			continue
		}
		m.fails[s.Name] = 0
		claimed[cur] = s.Name
		m.mu.Unlock()
	}
}

func (m *Manager) tickReselect(ctx context.Context, s Slot, cur string, slots []Slot, claimed map[string]string, now time.Time, generation uint64) {
	m.mu.Lock()
	if m.generation != generation {
		m.mu.Unlock()
		return
	}
	current := make(map[string]string, len(slots))
	for _, slot := range slots {
		node, err := m.ctrl.Current(ctx, slot.Group())
		if err != nil {
			continue
		}
		current[slot.Name] = node
	}
	inUse := map[string]bool{}
	for name, node := range current {
		if name != s.Name {
			inUse[node] = true
		}
	}
	for node := range claimed {
		inUse[node] = true
	}
	candidates := make([]string, 0, len(m.nodes))
	for _, node := range m.nodes {
		if node == cur || inUse[node] || m.badUntil[node].After(now) {
			continue
		}
		candidates = append(candidates, node)
	}
	m.opts.Shuffle(candidates)
	if len(candidates) > m.opts.MaxAttempts {
		candidates = candidates[:m.opts.MaxAttempts]
	}
	m.mu.Unlock()

	for _, candidate := range candidates {
		probeErr := m.ctrl.Delay(ctx, candidate, m.opts.ProbeURL, m.opts.ProbeTimeout)
		m.mu.Lock()
		if ctx.Err() != nil || m.generation != generation {
			m.mu.Unlock()
			return
		}
		live, err := m.ctrl.Current(ctx, s.Group())
		if err != nil || live != cur {
			m.mu.Unlock()
			return
		}
		if probeErr != nil {
			m.badUntil[candidate] = now.Add(m.opts.Cooldown)
			m.opts.Logf("vpngate: %s: candidate %q probe failed: %v", s.Name, candidate, probeErr)
			m.mu.Unlock()
			continue
		}
		available := true
		for _, slot := range slots {
			if slot.Name == s.Name {
				continue
			}
			used, currentErr := m.ctrl.Current(ctx, slot.Group())
			if currentErr == nil && used == candidate {
				available = false
				break
			}
		}
		if claimedBy, ok := claimed[candidate]; ok && claimedBy != s.Name {
			available = false
		}
		if m.badUntil[candidate].After(m.opts.Now()) {
			available = false
		}
		if !available {
			m.mu.Unlock()
			continue
		}
		if err := m.ctrl.Select(ctx, s.Group(), candidate); err != nil {
			m.opts.Logf("vpngate: %s: select %q failed: %v", s.Name, candidate, err)
			m.mu.Unlock()
			return
		}
		m.opts.Logf("vpngate: %s: switched %q -> %q", s.Name, cur, candidate)
		m.fails[s.Name] = 0
		claimed[candidate] = s.Name
		m.mu.Unlock()
		return
	}
	m.opts.Logf("vpngate: %s: no healthy free node (tried %d); keeping %q", s.Name, len(candidates), cur)
}

// Reselect moves slot s to a different random node that is healthy, out of
// cooldown and not used by any active slot. After a successful switch, its old
// node goes into cooldown so it is not handed to the next lease at once.
// s itself need not be active yet (a new lease reselects before it counts).
func (m *Manager) Reselect(ctx context.Context, s Slot) (string, error) {
	slots := m.active()
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.opts.Now()
	cur, err := m.ctrl.Current(ctx, s.Group())
	if err != nil {
		return "", err
	}
	current := map[string]string{}
	for _, o := range slots {
		if o.Name == s.Name {
			continue
		}
		node, err := m.ctrl.Current(ctx, o.Group())
		if err != nil {
			return "", err
		}
		current[o.Name] = node
	}
	node, ok := m.reselect(ctx, s, cur, current, map[string]string{}, now)
	if !ok {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", ErrNoHealthyNode
	}
	m.badUntil[cur] = now.Add(m.opts.Cooldown)
	return node, nil
}

func (m *Manager) reselect(ctx context.Context, s Slot, cur string, current, claimed map[string]string, now time.Time) (string, bool) {
	inUse := map[string]bool{}
	for name, node := range current {
		if name != s.Name {
			inUse[node] = true
		}
	}
	for node := range claimed {
		inUse[node] = true
	}
	candidates := make([]string, 0, len(m.nodes))
	for _, n := range m.nodes {
		if n == cur || inUse[n] || m.badUntil[n].After(now) {
			continue
		}
		candidates = append(candidates, n)
	}
	m.opts.Shuffle(candidates)
	if len(candidates) > m.opts.MaxAttempts {
		candidates = candidates[:m.opts.MaxAttempts]
	}
	for _, c := range candidates {
		if err := m.ctrl.Delay(ctx, c, m.opts.ProbeURL, m.opts.ProbeTimeout); err != nil {
			if ctx.Err() != nil {
				return "", false
			}
			m.badUntil[c] = now.Add(m.opts.Cooldown)
			m.opts.Logf("vpngate: %s: candidate %q probe failed: %v", s.Name, c, err)
			continue
		}
		if err := m.ctrl.Select(ctx, s.Group(), c); err != nil {
			m.opts.Logf("vpngate: %s: select %q failed: %v", s.Name, c, err)
			return "", false
		}
		m.opts.Logf("vpngate: %s: switched %q -> %q", s.Name, cur, c)
		current[s.Name] = c
		claimed[c] = s.Name
		m.fails[s.Name] = 0
		return c, true
	}
	// No DIRECT fallback: if cur is dead, this slot's traffic fails until a
	// later tick finds a node.
	m.opts.Logf("vpngate: %s: no healthy free node (tried %d); keeping %q", s.Name, len(candidates), cur)
	return "", false
}
