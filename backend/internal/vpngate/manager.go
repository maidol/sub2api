package vpngate

import (
	"context"
	"time"
)

// ManagerOptions tunes health checking and reselection.
type ManagerOptions struct {
	ProbeURL      string
	ProbeTimeout  time.Duration
	FailThreshold int           // consecutive probe failures before a slot moves
	Cooldown      time.Duration // a failed node is not picked again for this long
	MaxAttempts   int           // candidates probed per reselection
	Now           func() time.Time
	Shuffle       func([]string)
	Logf          func(format string, args ...any)
}

// Manager keeps every slot on a healthy node. A slot keeps its node for as
// long as the node passes probes; only after FailThreshold consecutive
// failures (or when two slots share a node) does it move to a random node
// that is healthy, not in cooldown, and not used by any other slot.
type Manager struct {
	ctrl     Controller
	slots    []Slot
	nodes    []string
	opts     ManagerOptions
	fails    map[string]int       // slot name -> consecutive failures of its node
	badUntil map[string]time.Time // node name -> end of cooldown
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

// Tick checks every slot once.
func (m *Manager) Tick(ctx context.Context) {
	now := m.opts.Now()
	current := make(map[string]string, len(m.slots)) // slot name -> node
	for _, s := range m.slots {
		node, err := m.ctrl.Current(ctx, s.Group())
		if err != nil {
			m.opts.Logf("vpngate: %s: read selection: %v", s.Name, err)
			continue
		}
		current[s.Name] = node
	}

	claimed := map[string]string{} // node -> slot that holds it this tick
	for _, s := range m.slots {
		cur, ok := current[s.Name]
		if !ok {
			continue
		}
		if owner, dup := claimed[cur]; dup {
			m.opts.Logf("vpngate: %s: node %q is already used by %s, reselecting", s.Name, cur, owner)
			m.reselect(ctx, s, cur, current, claimed, now)
			continue
		}
		if err := m.ctrl.Delay(ctx, cur, m.opts.ProbeURL, m.opts.ProbeTimeout); err != nil {
			m.fails[s.Name]++
			m.opts.Logf("vpngate: %s: node %q probe failed (%d/%d): %v",
				s.Name, cur, m.fails[s.Name], m.opts.FailThreshold, err)
			if m.fails[s.Name] < m.opts.FailThreshold {
				claimed[cur] = s.Name
				continue
			}
			m.badUntil[cur] = now.Add(m.opts.Cooldown)
			m.reselect(ctx, s, cur, current, claimed, now)
			continue
		}
		m.fails[s.Name] = 0
		claimed[cur] = s.Name
	}
}

func (m *Manager) reselect(ctx context.Context, s Slot, cur string, current, claimed map[string]string, now time.Time) {
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
			m.badUntil[c] = now.Add(m.opts.Cooldown)
			m.opts.Logf("vpngate: %s: candidate %q probe failed: %v", s.Name, c, err)
			continue
		}
		if err := m.ctrl.Select(ctx, s.Group(), c); err != nil {
			m.opts.Logf("vpngate: %s: select %q failed: %v", s.Name, c, err)
			return
		}
		m.opts.Logf("vpngate: %s: switched %q -> %q", s.Name, cur, c)
		current[s.Name] = c
		claimed[c] = s.Name
		m.fails[s.Name] = 0
		return
	}
	// No DIRECT fallback: if cur is dead, this slot's traffic fails until a
	// later tick finds a node.
	m.opts.Logf("vpngate: %s: no healthy free node (tried %d); keeping %q", s.Name, len(candidates), cur)
}
