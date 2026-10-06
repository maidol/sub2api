//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func exhaustedProvider() *fakePoolProvider {
	return &fakePoolProvider{handle: func(method, path, _ string) (int, string) {
		return http.StatusServiceUnavailable, `{"error":"pool_exhausted"}`
	}}
}

func TestAllocateLeasesANewSlotWithTheDefaultShareSettings(t *testing.T) {
	f := &fakePoolProvider{handle: func(string, string, string) (int, string) { return http.StatusOK, poolLeaseJSON }}
	svc, settings, repo := newPoolService(t, f)
	settings.m[SettingKeyProxyPoolDefaultShareable] = "true"
	settings.m[SettingKeyProxyPoolDefaultShareMax] = "4"

	p, shared, err := svc.Allocate(context.Background())
	if err != nil || shared {
		t.Fatalf("Allocate = %v, shared=%v", err, shared)
	}
	if got := repo.rows[p.ID]; !got.PoolShareable || got.PoolShareMax != 4 {
		t.Fatalf("stored share settings = %v/%d", got.PoolShareable, got.PoolShareMax)
	}
}

func TestAllocateFallsBackToTheLeastUsedSharedSlot(t *testing.T) {
	svc, _, repo := newPoolService(t, exhaustedProvider())
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, ExternalRef: "l-1", PoolShareable: true, PoolShareMax: 3, Status: StatusActive}
	repo.counts[1] = 2
	repo.rows[2] = &Proxy{ID: 2, ManagedBy: ProxyManagedByPool, ExternalRef: "l-2", PoolShareable: true, PoolShareMax: 0, Status: StatusActive}
	repo.counts[2] = 1
	repo.rows[3] = &Proxy{ID: 3, ManagedBy: ProxyManagedByPool, ExternalRef: "l-3", PoolShareable: true, PoolShareMax: 1, Status: StatusActive}
	repo.counts[3] = 0
	repo.rows[4] = &Proxy{ID: 4, ManagedBy: ProxyManagedByPool, ExternalRef: "l-4"} // not shared
	repo.rows[5] = &Proxy{ID: 5, Name: "hand-made", PoolShareable: true}            // not pool-managed

	p, shared, err := svc.Allocate(context.Background())
	if err != nil || !shared || p.ID != 3 {
		t.Fatalf("Allocate = %+v, shared=%v, err=%v; want proxy 3 (0 used)", p, shared, err)
	}

	repo.counts[3] = 1 // full now; 1 and 2 have room, 2 has fewer accounts
	p, _, _ = svc.Allocate(context.Background())
	if p.ID != 2 {
		t.Fatalf("Allocate picked %d, want 2", p.ID)
	}
}

func TestAllocateWithoutRoomReportsNoCapacity(t *testing.T) {
	svc, _, repo := newPoolService(t, exhaustedProvider())
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, ExternalRef: "l-1", PoolShareable: true, PoolShareMax: 1}
	repo.counts[1] = 1
	if _, _, err := svc.Allocate(context.Background()); !errors.Is(err, ErrProxyPoolNoCapacity) {
		t.Fatalf("err = %v, want ErrProxyPoolNoCapacity", err)
	}
}

func TestAllocateDoesNotShareOnOtherProviderErrors(t *testing.T) {
	f := &fakePoolProvider{handle: func(string, string, string) (int, string) {
		return http.StatusUnauthorized, `{"error":"unauthorized"}`
	}}
	svc, _, repo := newPoolService(t, f)
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, ExternalRef: "l-1", PoolShareable: true}
	if _, _, err := svc.Allocate(context.Background()); !errors.Is(err, ErrProxyPoolUnavailable) {
		t.Fatalf("err = %v, want ErrProxyPoolUnavailable", err)
	}
}

func TestListShareableReturnsSharedSlotsWithRoomByUse(t *testing.T) {
	svc, _, repo := newPoolService(t, nil)
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, PoolShareable: true, PoolShareMax: 5, Status: StatusActive}
	repo.counts[1] = 3
	repo.rows[2] = &Proxy{ID: 2, ManagedBy: ProxyManagedByPool, PoolShareable: true, Status: StatusActive}
	repo.counts[2] = 1
	repo.rows[3] = &Proxy{ID: 3, ManagedBy: ProxyManagedByPool, PoolShareable: true, PoolShareMax: 2, Status: StatusActive}
	repo.counts[3] = 2
	repo.rows[4] = &Proxy{ID: 4, ManagedBy: ProxyManagedByPool}

	got, err := svc.ListShareable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Proxy.ID != 2 || got[0].Used != 1 || got[1].Proxy.ID != 1 || got[1].Used != 3 {
		t.Fatalf("ListShareable = %+v", got)
	}
}

func TestShareDefaultsRoundTripAndValidate(t *testing.T) {
	svc, _, _ := newPoolService(t, nil)
	yes, four := true, 4
	if err := svc.UpdateShareDefaults(context.Background(), &yes, &four); err != nil {
		t.Fatal(err)
	}
	view, _ := svc.GetConfig(context.Background())
	if !view.DefaultShareable || view.DefaultShareMax != 4 {
		t.Fatalf("view = %+v", view)
	}
	neg := -1
	if err := svc.UpdateShareDefaults(context.Background(), nil, &neg); !errors.Is(err, ErrProxyPoolShareMaxInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestAllocateWithoutHealthyNodesKeepsThatError(t *testing.T) {
	f := &fakePoolProvider{handle: func(string, string, string) (int, string) {
		return http.StatusServiceUnavailable, `{"error":"no_healthy_node"}`
	}}
	svc, _, _ := newPoolService(t, f)
	if _, _, err := svc.Allocate(context.Background()); !errors.Is(err, ErrProxyPoolNoHealthyNode) {
		t.Fatalf("err = %v, want ErrProxyPoolNoHealthyNode", err)
	}
}
