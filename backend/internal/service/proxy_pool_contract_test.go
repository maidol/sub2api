//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vpngate"
)

// memController is a healthy in-memory Mihomo for the contract test.
type memController struct{ now map[string]string }

func (c *memController) Current(_ context.Context, group string) (string, error) {
	n, ok := c.now[group]
	if !ok {
		return "", fmt.Errorf("no group %q", group)
	}
	return n, nil
}

func (c *memController) Select(_ context.Context, group, node string) error {
	c.now[group] = node
	return nil
}

func (c *memController) Delay(context.Context, string, string, time.Duration) error { return nil }

// ProxyPoolService against the real vpngate lease API (only Mihomo is
// replaced): the JSON field names and status codes of both sides must agree.
func TestProxyPoolServiceAgainstTheRealLeaseAPI(t *testing.T) {
	const token, master = "contract-token-0123", "contract-master-0123"
	slots := vpngate.MakeSlots(2, 20001)
	ctrl := &memController{now: map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "A"}}
	store, _, err := vpngate.OpenLeaseStore(filepath.Join(t.TempDir(), "leases.json"), slots)
	if err != nil {
		t.Fatal(err)
	}
	// Six nodes: with a 1h cooldown, rotating and re-leasing below cool down
	// three of them, and the other lease holds a fourth.
	nodes := []vpngate.Node{{Name: "A"}, {Name: "B"}, {Name: "C"}, {Name: "D"}, {Name: "E"}, {Name: "F"}}
	mgr := vpngate.NewManager(ctrl, slots, nodes, vpngate.ManagerOptions{
		ProbeTimeout: time.Second, FailThreshold: 3, Cooldown: time.Hour, MaxAttempts: 5,
		Active: store.ActiveSlots,
	})
	api := &vpngate.API{Token: token, PublicHost: "vpngate", Master: master, Store: store, Mgr: mgr, NodeCount: len(nodes)}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	settings := &poolSettingRepo{m: map[string]string{}}
	repo := newPoolProxyRepo()
	repo.now = time.Now()
	svc := NewProxyPoolService(settings, repo, srv.URL, token, 0)
	ctx := context.Background()

	health, err := svc.Health(ctx)
	if err != nil || health["slots"] != float64(2) {
		t.Fatalf("Health = %v, %v", health, err)
	}

	p, _, err := svc.Allocate(ctx)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	leases := store.List()
	if len(leases) != 1 {
		t.Fatalf("provider leases = %+v", leases)
	}
	if p.ExternalRef != leases[0].ID || p.Host != "vpngate" || p.Port != 20001 || p.Username != "slot01" ||
		p.Password != vpngate.SlotPassword(master, "slot01") || p.Protocol != "http" || p.ManagedBy != ProxyManagedByPool {
		t.Fatalf("stored proxy %+v does not match provider lease %+v", *p, leases[0])
	}
	before := ctrl.now["sub2api-slot01"]
	node, err := svc.Rotate(ctx, p.ID)
	if err != nil || node == "" || node == before || ctrl.now["sub2api-slot01"] != node {
		t.Fatalf("Rotate = %q, %v (was %q, slot now %q)", node, err, before, ctrl.now["sub2api-slot01"])
	}

	// A second lease gets the other slot; the pool is then exhausted and no
	// slot is shared.
	if _, _, err := svc.Allocate(ctx); err != nil {
		t.Fatalf("second Allocate: %v", err)
	}
	if _, _, err := svc.Allocate(ctx); err != ErrProxyPoolNoCapacity {
		t.Fatalf("third Allocate err = %v, want ErrProxyPoolNoCapacity", err)
	}

	// A day later nobody uses the first proxy: Reconcile gives its slot back.
	repo.rows[p.ID].CreatedAt = time.Now().Add(-25 * time.Hour)
	svc.now = time.Now
	rows, orphans, err := svc.Reconcile(ctx)
	if err != nil || rows != 1 || orphans != 0 {
		t.Fatalf("Reconcile = %d, %d, %v", rows, orphans, err)
	}
	if left := store.List(); len(left) != 1 || left[0].Slot != "slot02" {
		t.Fatalf("provider leases after reconcile = %+v, want only slot02", left)
	}
	if _, _, err := svc.Allocate(ctx); err != nil {
		t.Fatalf("slot01 must be leasable again: %v", err)
	}
}
