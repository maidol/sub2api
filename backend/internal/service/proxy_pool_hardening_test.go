//go:build unit

package service

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProxyPoolReconcileKeepsRowsUsedAsABackup(t *testing.T) {
	old := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	f := &fakePoolProvider{handle: func(method, path, body string) (int, string) {
		switch {
		case method == http.MethodGet && path == "/v1/leases":
			return http.StatusOK, `{"leases":[` +
				`{"lease_id":"l-backup","client_ref":"sub2api-a","created_at":"2026-09-29T00:00:00Z"},` +
				`{"lease_id":"l-unused","client_ref":"sub2api-b","created_at":"2026-09-29T00:00:00Z"}]}`
		case method == http.MethodDelete:
			return http.StatusNoContent, ""
		}
		return http.StatusNotFound, ""
	}}
	svc, _, repo := newPoolService(t, f)
	backup := int64(1)
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, ExternalRef: "l-backup", CreatedAt: old}
	repo.rows[2] = &Proxy{ID: 2, ManagedBy: ProxyManagedByPool, ExternalRef: "l-unused", CreatedAt: old}
	repo.rows[3] = &Proxy{ID: 3, Name: "hand-made", FallbackMode: FallbackModeProxy, BackupProxyID: &backup, CreatedAt: old}

	rows, orphans, err := svc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rows != 1 || orphans != 0 {
		t.Fatalf("released rows=%d orphans=%d, want 1 and 0", rows, orphans)
	}
	if want := []int64{2}; !reflect.DeepEqual(repo.deleted, want) {
		t.Fatalf("deleted rows = %v, want %v (row 1 is proxy 3's backup)", repo.deleted, want)
	}
	for _, s := range f.seen {
		if strings.HasPrefix(s, "DELETE /v1/leases/l-backup") {
			t.Fatalf("the lease behind a backup proxy was released: %v", f.seen)
		}
	}
}

// captureLog sends the standard logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

const poolSecretLeaseID = "LEASEID-7f3a9c"

func TestProxyPoolLogsNeverContainALeaseID(t *testing.T) {
	lease := strings.Replace(poolLeaseJSON, `"lease_id":"l-1"`, `"lease_id":"`+poolSecretLeaseID+`"`, 1)
	rotateBody := lease
	f := &fakePoolProvider{handle: func(method, path, body string) (int, string) {
		switch {
		case method == http.MethodPost && path == "/v1/leases":
			return http.StatusOK, lease
		case method == http.MethodPost && strings.HasSuffix(path, "/rotate"):
			return http.StatusOK, rotateBody
		case method == http.MethodGet && path == "/v1/leases":
			return http.StatusOK, `{"leases":[{"lease_id":"` + poolSecretLeaseID + `-orphan","client_ref":"sub2api-z","created_at":"2026-09-29T00:00:00Z"}]}`
		case method == http.MethodDelete:
			return http.StatusNoContent, ""
		}
		return http.StatusNotFound, ""
	}}
	svc, _, repo := newPoolService(t, f)
	buf := captureLog(t)

	// leased, rotated, and released as unused once it is old
	p, err := svc.Lease(context.Background())
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if _, err := svc.Rotate(context.Background(), p.ID); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	// a rotate answer that is not JSON logs a decode error
	rotateBody = "not json"
	if _, err := svc.Rotate(context.Background(), p.ID); err == nil {
		t.Fatal("Rotate with a broken answer: expected an error")
	}
	repo.rows[p.ID].CreatedAt = time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	if rows, orphans, err := svc.Reconcile(context.Background()); err != nil || rows != 1 || orphans != 1 {
		t.Fatalf("Reconcile = %d, %d, %v", rows, orphans, err)
	}
	// an incomplete lease is logged and released
	lease = strings.Replace(lease, `"password":"0123456789abcdef0123456789abcdef",`, "", 1)
	if _, err := svc.Lease(context.Background()); err == nil {
		t.Fatal("incomplete lease: expected an error")
	}

	// a provider that cannot be reached: the transport error repeats the URL
	down := NewProxyPoolService(&poolSettingRepo{m: map[string]string{}}, newPoolProxyRepo(), "http://127.0.0.1:1", "env-token-0123456789", 0)
	down.proxyRepo.(*poolProxyRepo).rows[7] = &Proxy{ID: 7, ManagedBy: ProxyManagedByPool, ExternalRef: poolSecretLeaseID}
	if _, err := down.Rotate(context.Background(), 7); err == nil {
		t.Fatal("unreachable provider: expected an error")
	}

	out := buf.String()
	if strings.Contains(out, poolSecretLeaseID) {
		t.Fatalf("a lease ID reached the log:\n%s", out)
	}
	for _, want := range []string{"leased proxy", "rotated proxy", "decode", "released unused proxy", "released an orphan lease", "incomplete lease", "/v1/leases/{id}/rotate"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q, so this test did not reach that line:\n%s", want, out)
		}
	}
}
