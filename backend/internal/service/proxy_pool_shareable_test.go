//go:build unit

package service

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestProxyPoolListShareableFiltersInactiveAndExpired(t *testing.T) {
	svc, _, repo := newPoolService(t, nil)
	now := svc.now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, PoolShareable: true, PoolShareMax: 5, Status: StatusActive}
	repo.rows[2] = &Proxy{ID: 2, ManagedBy: ProxyManagedByPool, PoolShareable: true, PoolShareMax: 5, Status: StatusDisabled}
	repo.rows[3] = &Proxy{ID: 3, ManagedBy: ProxyManagedByPool, PoolShareable: true, PoolShareMax: 5, Status: StatusActive, ExpiresAt: &past}
	repo.rows[4] = &Proxy{ID: 4, ManagedBy: ProxyManagedByPool, PoolShareable: true, PoolShareMax: 5, Status: StatusActive, ExpiresAt: &future}
	repo.rows[5] = &Proxy{ID: 5, ManagedBy: "manual", PoolShareable: true, PoolShareMax: 5, Status: StatusActive}

	candidates, err := svc.ListShareable(context.Background())
	if err != nil {
		t.Fatalf("ListShareable: %v", err)
	}
	var ids []int64
	for _, candidate := range candidates {
		ids = append(ids, candidate.Proxy.ID)
	}
	if want := []int64{1, 4}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("candidate IDs = %v, want %v", ids, want)
	}
}

func TestProxyPoolAllocateFallbackUsesEligibleShareable(t *testing.T) {
	f := &fakePoolProvider{handle: func(method, path, body string) (int, string) {
		if method == http.MethodPost && path == "/v1/leases" {
			return http.StatusServiceUnavailable, `{"error":"pool_exhausted"}`
		}
		return http.StatusNotFound, ""
	}}
	svc, _, repo := newPoolService(t, f)
	now := svc.now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, PoolShareable: true, PoolShareMax: 5, Status: StatusActive, ExpiresAt: &past}
	repo.rows[2] = &Proxy{ID: 2, ManagedBy: ProxyManagedByPool, PoolShareable: true, PoolShareMax: 5, Status: StatusDisabled}
	repo.rows[3] = &Proxy{ID: 3, ManagedBy: ProxyManagedByPool, PoolShareable: true, PoolShareMax: 5, Status: StatusActive, ExpiresAt: &future}

	proxy, shared, err := svc.Allocate(context.Background())
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if !shared || proxy == nil || proxy.ID != 3 {
		t.Fatalf("Allocate = proxy %v shared=%v, want active unexpired proxy 3 shared=true", proxy, shared)
	}
}
