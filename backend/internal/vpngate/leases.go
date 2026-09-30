package vpngate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrPoolExhausted means every slot is leased.
var ErrPoolExhausted = errors.New("pool exhausted")

// Lease binds one slot to one client (Sub2API passes a reference of its own).
type Lease struct {
	ID        string    `json:"id"`
	ClientRef string    `json:"client_ref"`
	Protocol  string    `json:"protocol"` // "http" or "socks5"; both work on the slot's port
	Slot      string    `json:"slot"`
	CreatedAt time.Time `json:"created_at"`
}

type leaseFile struct {
	Version int     `json:"version"`
	Leases  []Lease `json:"leases"`
}

// LeaseStore keeps leases in memory and in a JSON file under the state
// directory, so leases survive a sidecar restart.
type LeaseStore struct {
	mu     sync.Mutex
	path   string
	slots  []Slot
	leases map[string]Lease // by lease ID
	Now    func() time.Time
	NewID  func() string
}

// OpenLeaseStore loads path (a missing file means no leases). Leases whose
// slot no longer exists (VPNGATE_SLOTS was lowered) are dropped and returned.
func OpenLeaseStore(path string, slots []Slot) (*LeaseStore, []Lease, error) {
	st := &LeaseStore{
		path:   path,
		slots:  slots,
		leases: map[string]Lease{},
		Now:    time.Now,
		NewID:  newLeaseID,
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var f leaseFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	known := map[string]bool{}
	for _, s := range slots {
		known[s.Name] = true
	}
	used := map[string]bool{}
	var dropped []Lease
	for _, l := range f.Leases {
		if !known[l.Slot] || used[l.Slot] || l.ID == "" {
			dropped = append(dropped, l)
			continue
		}
		used[l.Slot] = true
		st.leases[l.ID] = l
	}
	if len(dropped) > 0 {
		if err := st.saveLocked(); err != nil {
			return nil, nil, err
		}
	}
	return st, dropped, nil
}

func newLeaseID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return "l-" + hex.EncodeToString(b)
}

// SlotByName returns the slot with that name.
func (st *LeaseStore) SlotByName(name string) (Slot, bool) {
	for _, s := range st.slots {
		if s.Name == name {
			return s, true
		}
	}
	return Slot{}, false
}

// Get returns a lease by ID.
func (st *LeaseStore) Get(id string) (Lease, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	l, ok := st.leases[id]
	return l, ok
}

// ByClientRef returns the lease held by clientRef, if any.
func (st *LeaseStore) ByClientRef(clientRef string) (Lease, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, l := range st.leases {
		if l.ClientRef == clientRef {
			return l, true
		}
	}
	return Lease{}, false
}

// FreeSlot returns the lowest-numbered slot without a lease.
func (st *LeaseStore) FreeSlot() (Slot, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	used := map[string]bool{}
	for _, l := range st.leases {
		used[l.Slot] = true
	}
	for _, s := range st.slots {
		if !used[s.Name] {
			return s, nil
		}
	}
	return Slot{}, ErrPoolExhausted
}

// Add records a new lease on slot and persists it.
func (st *LeaseStore) Add(clientRef, protocol string, slot Slot) (Lease, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, l := range st.leases {
		if l.Slot == slot.Name {
			return Lease{}, fmt.Errorf("slot %s is already leased", slot.Name)
		}
	}
	l := Lease{ID: st.NewID(), ClientRef: clientRef, Protocol: protocol, Slot: slot.Name, CreatedAt: st.Now().UTC()}
	st.leases[l.ID] = l
	if err := st.saveLocked(); err != nil {
		delete(st.leases, l.ID)
		return Lease{}, err
	}
	return l, nil
}

// Remove deletes a lease and persists the change. It reports whether the
// lease existed.
func (st *LeaseStore) Remove(id string) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	l, ok := st.leases[id]
	if !ok {
		return false, nil
	}
	delete(st.leases, id)
	if err := st.saveLocked(); err != nil {
		st.leases[id] = l
		return false, err
	}
	return true, nil
}

// List returns all leases ordered by slot port.
func (st *LeaseStore) List() []Lease {
	st.mu.Lock()
	defer st.mu.Unlock()
	order := map[string]int{}
	for i, s := range st.slots {
		order[s.Name] = i
	}
	out := make([]Lease, 0, len(st.leases))
	for _, l := range st.leases {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i].Slot] < order[out[j].Slot] })
	return out
}

// ActiveSlots returns the leased slots; it is the Manager's Active option.
func (st *LeaseStore) ActiveSlots() []Slot {
	leases := st.List()
	out := make([]Slot, 0, len(leases))
	for _, l := range leases {
		if s, ok := st.SlotByName(l.Slot); ok {
			out = append(out, s)
		}
	}
	return out
}

func (st *LeaseStore) saveLocked() error {
	f := leaseFile{Version: 1, Leases: make([]Lease, 0, len(st.leases))}
	for _, l := range st.leases {
		f.Leases = append(f.Leases, l)
	}
	sort.Slice(f.Leases, func(i, j int) bool { return f.Leases[i].ID < f.Leases[j].ID })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(st.path), ".leases-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, st.path)
}
