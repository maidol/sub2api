//go:build unit

package vpngate

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func openTestStore(t *testing.T, path string, slots []Slot) *LeaseStore {
	t.Helper()
	st, dropped, err := OpenLeaseStore(path, slots)
	if err != nil {
		t.Fatalf("OpenLeaseStore: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("unexpected dropped leases: %+v", dropped)
	}
	n := 0
	st.NewID = func() string { n++; return fmt.Sprintf("l-%d", n) }
	st.Now = func() time.Time { return time.Unix(1700000000, 0) }
	return st
}

func TestLeaseStoreAllocatesLowestFreeSlotAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	slots := MakeSlots(2, 20001)
	st := openTestStore(t, path, slots)

	s, err := st.FreeSlot()
	if err != nil || s.Name != "slot01" {
		t.Fatalf("FreeSlot = %+v, %v", s, err)
	}
	l1, err := st.Add("acct-a", "http", s)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := st.Add("acct-x", "http", s); err == nil {
		t.Fatal("a slot must not be leased twice")
	}
	s2, _ := st.FreeSlot()
	l2, _ := st.Add("acct-b", "socks5", s2)
	if _, err := st.FreeSlot(); err != ErrPoolExhausted {
		t.Fatalf("FreeSlot with every slot leased = %v, want ErrPoolExhausted", err)
	}
	if got, ok := st.ByClientRef("acct-b"); !ok || got.ID != l2.ID {
		t.Fatalf("ByClientRef = %+v, %v", got, ok)
	}
	if got := st.ActiveSlots(); !reflect.DeepEqual(got, slots) {
		t.Fatalf("ActiveSlots = %+v", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lease file mode = %v, want 0600", info.Mode().Perm())
	}

	// A restart sees the same leases.
	re := openTestStore(t, path, slots)
	if !reflect.DeepEqual(re.List(), []Lease{l1, l2}) {
		t.Fatalf("after reopen = %+v, want %+v", re.List(), []Lease{l1, l2})
	}

	existed, err := re.Remove(l1.ID)
	if err != nil || !existed {
		t.Fatalf("Remove = %v, %v", existed, err)
	}
	if existed, _ := re.Remove(l1.ID); existed {
		t.Fatal("removing twice must report false")
	}
	if s, _ := re.FreeSlot(); s.Name != "slot01" {
		t.Fatalf("released slot must be free again, got %+v", s)
	}
	if again := openTestStore(t, path, slots); !reflect.DeepEqual(again.List(), []Lease{l2}) {
		t.Fatalf("removal not persisted: %+v", again.List())
	}
}

func TestLeaseStoreDropsLeasesOnVanishedSlots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	st := openTestStore(t, path, MakeSlots(3, 20001))
	for i, s := range MakeSlots(3, 20001) {
		if _, err := st.Add(fmt.Sprintf("acct-%d", i), "http", s); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	re, dropped, err := OpenLeaseStore(path, MakeSlots(2, 20001))
	if err != nil {
		t.Fatalf("OpenLeaseStore: %v", err)
	}
	if len(dropped) != 1 || dropped[0].Slot != "slot03" {
		t.Fatalf("dropped = %+v, want the slot03 lease", dropped)
	}
	if len(re.List()) != 2 {
		t.Fatalf("kept = %+v", re.List())
	}
	// The drop is persisted, so it is reported once, not on every start.
	if _, dropped, _ := OpenLeaseStore(path, MakeSlots(2, 20001)); len(dropped) != 0 {
		t.Fatalf("dropped again on the next start: %+v", dropped)
	}
}
