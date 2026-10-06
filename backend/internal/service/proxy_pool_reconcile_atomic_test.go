//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"
)

type proxyPoolDeleteResultRepo struct {
	*poolProxyRepo
	deleted bool
	err     error
}

func (r *proxyPoolDeleteResultRepo) DeletePoolProxyIfUnused(_ context.Context, id int64) (bool, error) {
	if r.err != nil || !r.deleted {
		return r.deleted, r.err
	}
	return r.poolProxyRepo.DeletePoolProxyIfUnused(context.Background(), id)
}

func TestProxyPoolReconcileSkipsProviderReleaseWhenLocalDeleteDoesNotHappen(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "still referenced"},
		{name: "repository failure", err: errors.New("database unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakePoolProvider{handle: func(method, path, body string) (int, string) {
				if method == http.MethodGet && path == "/v1/leases" {
					return http.StatusOK, `{"leases":[{"lease_id":"l-old","client_ref":"sub2api-a","created_at":"2026-09-29T00:00:00Z"}]}`
				}
				return http.StatusNoContent, ""
			}}
			svc, _, repo := newPoolService(t, f)
			atomicRepo := &proxyPoolDeleteResultRepo{poolProxyRepo: repo, err: tc.err}
			svc.proxyRepo = atomicRepo
			atomicRepo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, ExternalRef: "l-old", CreatedAt: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)}

			rows, _, err := svc.Reconcile(context.Background())
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("Reconcile error = %v, want %v", err, tc.err)
				}
			} else if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if rows != 0 {
				t.Fatalf("locally deleted rows = %d, want 0", rows)
			}
			for _, request := range f.seen {
				if len(request) >= len("DELETE ") && request[:len("DELETE ")] == "DELETE " {
					t.Fatalf("provider release must not happen when local deletion did not commit: %v", f.seen)
				}
			}
		})
	}
}

func TestProxyPoolReconcileCountsLocalDeleteWhenProviderReleaseFails(t *testing.T) {
	f := &fakePoolProvider{handle: func(method, path, body string) (int, string) {
		if method == http.MethodGet && path == "/v1/leases" {
			return http.StatusOK, `{"leases":[{"lease_id":"l-old","client_ref":"sub2api-a","created_at":"2026-09-29T00:00:00Z"}]}`
		}
		if method == http.MethodDelete {
			return http.StatusInternalServerError, `{"error":"provider unavailable"}`
		}
		return http.StatusNotFound, ""
	}}
	svc, _, repo := newPoolService(t, f)
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, ExternalRef: "l-old", CreatedAt: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)}

	rows, orphans, err := svc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rows != 1 || orphans != 0 {
		t.Fatalf("Reconcile counts = rows %d orphans %d, want local delete 1 and no orphan release this round", rows, orphans)
	}
	if _, exists := repo.rows[1]; exists {
		t.Fatal("local proxy row should remain deleted after provider release failure")
	}
	var deletes []string
	for _, request := range f.seen {
		if len(request) >= len("DELETE ") && request[:len("DELETE ")] == "DELETE " {
			deletes = append(deletes, request[:len("DELETE /v1/leases/l-old")])
		}
	}
	if want := []string{"DELETE /v1/leases/l-old"}; !reflect.DeepEqual(deletes, want) {
		t.Fatalf("provider DELETE requests = %v, want attempted release", deletes)
	}
}
