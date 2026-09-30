//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// poolSettingRepo is an in-memory SettingRepository.
type poolSettingRepo struct{ m map[string]string }

func (r *poolSettingRepo) Get(_ context.Context, key string) (*Setting, error) {
	v, ok := r.m[key]
	if !ok {
		return nil, ErrSettingNotFound
	}
	return &Setting{Key: key, Value: v}, nil
}
func (r *poolSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	v, ok := r.m[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}
func (r *poolSettingRepo) Set(_ context.Context, key, value string) error {
	r.m[key] = value
	return nil
}
func (r *poolSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := r.m[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}
func (r *poolSettingRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	for k, v := range settings {
		r.m[k] = v
	}
	return nil
}
func (r *poolSettingRepo) GetAll(_ context.Context) (map[string]string, error) { return r.m, nil }
func (r *poolSettingRepo) Delete(_ context.Context, key string) error {
	if _, ok := r.m[key]; !ok {
		return ErrSettingNotFound
	}
	delete(r.m, key)
	return nil
}

// poolProxyRepo keeps proxies in memory; every other ProxyRepository method
// panics through the embedded proxyRepoStub.
type poolProxyRepo struct {
	*proxyRepoStub
	rows      map[int64]*Proxy
	counts    map[int64]int64
	nextID    int64
	createErr error
	deleted   []int64
	now       time.Time // CreatedAt of new rows, as the database would set it
}

func newPoolProxyRepo() *poolProxyRepo {
	return &poolProxyRepo{proxyRepoStub: &proxyRepoStub{}, rows: map[int64]*Proxy{}, counts: map[int64]int64{}, nextID: 100,
		now: time.Date(2026, 9, 30, 11, 59, 0, 0, time.UTC)}
}

func (r *poolProxyRepo) Create(_ context.Context, p *Proxy) error {
	if r.createErr != nil {
		return r.createErr
	}
	r.nextID++
	p.ID = r.nextID
	p.CreatedAt = r.now
	cp := *p
	r.rows[p.ID] = &cp
	return nil
}
func (r *poolProxyRepo) GetByID(_ context.Context, id int64) (*Proxy, error) {
	p, ok := r.rows[id]
	if !ok {
		return nil, ErrProxyNotFound
	}
	cp := *p
	return &cp, nil
}
func (r *poolProxyRepo) ListAllForFallback(context.Context) ([]Proxy, error) {
	ids := make([]int64, 0, len(r.rows))
	for id := range r.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]Proxy, 0, len(ids))
	for _, id := range ids {
		out = append(out, *r.rows[id])
	}
	return out, nil
}
func (r *poolProxyRepo) CountAccountsByProxyID(_ context.Context, id int64) (int64, error) {
	return r.counts[id], nil
}
func (r *poolProxyRepo) Delete(_ context.Context, id int64) error {
	delete(r.rows, id)
	r.deleted = append(r.deleted, id)
	return nil
}

// fakePoolProvider is the vpngate lease API. handle decides the answer per
// request; every request is recorded as "METHOD /path body".
type fakePoolProvider struct {
	mu     sync.Mutex
	seen   []string
	auth   []string
	handle func(method, path, body string) (int, string)
}

func (f *fakePoolProvider) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(data)))
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		status, body := f.handle(r.Method, r.URL.Path, string(data))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const poolLeaseJSON = `{"lease_id":"l-1","client_ref":"sub2api-x","protocol":"http","host":"vpngate","port":20003,` +
	`"username":"slot03","password":"0123456789abcdef0123456789abcdef","node":"VPNGate-JP-1","created_at":"2026-09-30T00:00:00Z"}`

func newPoolService(t *testing.T, f *fakePoolProvider) (*ProxyPoolService, *poolSettingRepo, *poolProxyRepo) {
	t.Helper()
	settings := &poolSettingRepo{m: map[string]string{}}
	repo := newPoolProxyRepo()
	url := ""
	if f != nil {
		url = f.server(t).URL
	}
	svc := NewProxyPoolService(settings, repo, url, "env-token-0123456789", 0)
	svc.newRef = func() string { return "sub2api-ref1" }
	svc.now = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
	return svc, settings, repo
}

func TestProxyPoolConfigPrefersSavedValuesAndNeverReturnsTheToken(t *testing.T) {
	svc, settings, _ := newPoolService(t, nil)
	svc.envURL = "http://env-provider:20000"
	ctx := context.Background()

	view, err := svc.GetConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ProxyPoolConfigView{URL: "http://env-provider:20000", URLSource: "env", TokenSource: "env", TokenConfigured: true}); view != want {
		t.Fatalf("env-only view = %+v, want %+v", view, want)
	}

	token := "ui-token-0123456789"
	view, err = svc.UpdateConfig(ctx, "http://vpngate:20000/", &token)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ProxyPoolConfigView{URL: "http://vpngate:20000", URLSource: "setting", TokenSource: "setting", TokenConfigured: true}); view != want {
		t.Fatalf("after save = %+v, want %+v", view, want)
	}
	if settings.m[SettingKeyProxyPoolToken] != token {
		t.Fatalf("token not saved: %v", settings.m)
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "env-token") {
		t.Fatalf("config view leaks a token: %s", raw)
	}

	// Empty URL removes the saved URL; nil token keeps the saved token.
	view, _ = svc.UpdateConfig(ctx, "", nil)
	if view.URLSource != "env" || view.TokenSource != "setting" {
		t.Fatalf("after clearing URL = %+v", view)
	}
	// "" removes the saved token.
	empty := ""
	view, _ = svc.UpdateConfig(ctx, "", &empty)
	if view.TokenSource != "env" {
		t.Fatalf("after clearing token = %+v", view)
	}
}

func TestProxyPoolUpdateConfigRejectsBadURLs(t *testing.T) {
	svc, settings, _ := newPoolService(t, nil)
	for _, bad := range []string{"ftp://vpngate:20000", "http://", "vpngate:20000", "http://user:pw@vpngate:20000", "http://vpngate:20000/v1", "http://vpngate:20000?x=1"} {
		if _, err := svc.UpdateConfig(context.Background(), bad, nil); !errors.Is(err, ErrProxyPoolURLInvalid) {
			t.Errorf("%q: err = %v, want ErrProxyPoolURLInvalid", bad, err)
		}
	}
	if _, ok := settings.m[SettingKeyProxyPoolURL]; ok {
		t.Fatalf("a rejected URL must not be saved: %v", settings.m)
	}
}

func TestProxyPoolLeaseStoresAPoolManagedProxy(t *testing.T) {
	f := &fakePoolProvider{handle: func(method, path, body string) (int, string) {
		return http.StatusOK, poolLeaseJSON
	}}
	svc, _, repo := newPoolService(t, f)
	p, err := svc.Lease(context.Background())
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	want := Proxy{
		ID: 101, Name: "Proxy pool · slot03", Protocol: "http", Host: "vpngate", Port: 20003,
		Username: "slot03", Password: "0123456789abcdef0123456789abcdef", Status: StatusActive,
		FallbackMode: FallbackModeNone, ExpiryWarnDays: 7, ManagedBy: ProxyManagedByPool, ExternalRef: "l-1",
		CreatedAt: repo.now,
	}
	if !reflect.DeepEqual(*p, want) || !reflect.DeepEqual(*repo.rows[101], want) {
		t.Fatalf("proxy = %+v\nstored = %+v\nwant   %+v", *p, *repo.rows[101], want)
	}
	if want := []string{`POST /v1/leases {"client_ref":"sub2api-ref1","protocol":"http"}`}; !reflect.DeepEqual(f.seen, want) {
		t.Fatalf("requests = %v, want %v", f.seen, want)
	}
	if f.auth[0] != "Bearer env-token-0123456789" {
		t.Fatalf("Authorization = %q", f.auth[0])
	}
}

func TestProxyPoolLeaseMapsProviderErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusServiceUnavailable, `{"error":"pool_exhausted"}`, ErrProxyPoolExhausted},
		{http.StatusServiceUnavailable, `{"error":"no_healthy_node"}`, ErrProxyPoolNoHealthyNode},
		{http.StatusUnauthorized, `{"error":"unauthorized"}`, ErrProxyPoolUnavailable},
		{http.StatusInternalServerError, `oops`, ErrProxyPoolUnavailable},
	}
	for _, c := range cases {
		f := &fakePoolProvider{handle: func(string, string, string) (int, string) { return c.status, c.body }}
		svc, _, repo := newPoolService(t, f)
		if _, err := svc.Lease(context.Background()); !errors.Is(err, c.want) {
			t.Errorf("%d %s: err = %v, want %v", c.status, c.body, err, c.want)
		}
		if len(repo.rows) != 0 {
			t.Errorf("%d %s: a failed lease must not create a proxy", c.status, c.body)
		}
	}
}

func TestProxyPoolLeaseWithoutProviderConfigDoesNotCallOut(t *testing.T) {
	svc, _, repo := newPoolService(t, nil)
	svc.envToken = ""
	if _, err := svc.Lease(context.Background()); !errors.Is(err, ErrProxyPoolNotConfigured) {
		t.Fatalf("err = %v, want ErrProxyPoolNotConfigured", err)
	}
	if len(repo.rows) != 0 {
		t.Fatal("no proxy may be created")
	}
}

func TestProxyPoolLeaseReleasesTheLeaseItCannotUse(t *testing.T) {
	for name, tc := range map[string]struct {
		lease     string
		createErr error
	}{
		"row not stored":   {lease: poolLeaseJSON, createErr: errors.New("db down")},
		"incomplete lease": {lease: strings.Replace(poolLeaseJSON, `"password":"0123456789abcdef0123456789abcdef",`, "", 1)},
	} {
		f := &fakePoolProvider{handle: func(method, path, body string) (int, string) {
			if method == http.MethodDelete {
				return http.StatusNoContent, ""
			}
			return http.StatusOK, tc.lease
		}}
		svc, _, repo := newPoolService(t, f)
		repo.createErr = tc.createErr
		if _, err := svc.Lease(context.Background()); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if last := f.seen[len(f.seen)-1]; last != "DELETE /v1/leases/l-1 " {
			t.Fatalf("%s: last request = %q, want the lease released", name, last)
		}
	}
}

func TestProxyPoolRotate(t *testing.T) {
	f := &fakePoolProvider{handle: func(method, path, body string) (int, string) {
		if path == "/v1/leases/l-gone/rotate" {
			return http.StatusNotFound, `{"error":"not_found"}`
		}
		return http.StatusOK, strings.Replace(poolLeaseJSON, "VPNGate-JP-1", "VPNGate-JP-2", 1)
	}}
	svc, _, repo := newPoolService(t, f)
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, ExternalRef: "l-1"}
	repo.rows[2] = &Proxy{ID: 2, Name: "hand-made"}
	repo.rows[3] = &Proxy{ID: 3, ManagedBy: ProxyManagedByPool, ExternalRef: "l-gone"}

	node, err := svc.Rotate(context.Background(), 1)
	if err != nil || node != "VPNGate-JP-2" {
		t.Fatalf("Rotate = %q, %v", node, err)
	}
	if _, err := svc.Rotate(context.Background(), 2); !errors.Is(err, ErrProxyNotPoolManaged) {
		t.Fatalf("rotating a hand-made proxy: err = %v", err)
	}
	if _, err := svc.Rotate(context.Background(), 3); !errors.Is(err, ErrProxyPoolLeaseGone) {
		t.Fatalf("rotating a lease the provider lost: err = %v", err)
	}
	if want := []string{"POST /v1/leases/l-1/rotate ", "POST /v1/leases/l-gone/rotate "}; !reflect.DeepEqual(f.seen, want) {
		t.Fatalf("requests = %v, want %v (none for the hand-made proxy)", f.seen, want)
	}
}

func TestProxyPoolReconcileReleasesOnlyOldUnusedRowsAndOrphanLeases(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	young := now.Add(-2 * time.Hour)
	f := &fakePoolProvider{handle: func(method, path, body string) (int, string) {
		switch {
		case method == http.MethodGet && path == "/v1/leases":
			return http.StatusOK, `{"leases":[` +
				`{"lease_id":"l-old-unused","client_ref":"sub2api-a","created_at":"2026-09-29T00:00:00Z"},` +
				`{"lease_id":"l-old-used","client_ref":"sub2api-b","created_at":"2026-09-29T00:00:00Z"},` +
				`{"lease_id":"l-young","client_ref":"sub2api-c","created_at":"2026-09-30T10:00:00Z"},` +
				`{"lease_id":"l-stuck","client_ref":"sub2api-e","created_at":"2026-09-29T00:00:00Z"},` +
				`{"lease_id":"l-orphan-old","client_ref":"sub2api-f","created_at":"2026-09-30T11:00:00Z"},` +
				`{"lease_id":"l-orphan-new","client_ref":"sub2api-g","created_at":"2026-09-30T11:55:00Z"},` +
				`{"lease_id":"l-foreign","client_ref":"someone-else","created_at":"2026-09-29T00:00:00Z"}]}`
		case method == http.MethodDelete && path == "/v1/leases/l-stuck":
			return http.StatusInternalServerError, `{"error":"internal"}`
		case method == http.MethodDelete:
			return http.StatusNoContent, ""
		}
		return http.StatusNotFound, ""
	}}
	svc, _, repo := newPoolService(t, f)
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, ExternalRef: "l-old-unused", CreatedAt: old}
	repo.rows[2] = &Proxy{ID: 2, ManagedBy: ProxyManagedByPool, ExternalRef: "l-old-used", CreatedAt: old}
	repo.counts[2] = 1
	repo.rows[3] = &Proxy{ID: 3, ManagedBy: ProxyManagedByPool, ExternalRef: "l-young", CreatedAt: young}
	repo.rows[4] = &Proxy{ID: 4, Name: "hand-made", CreatedAt: old}
	repo.rows[5] = &Proxy{ID: 5, ManagedBy: ProxyManagedByPool, ExternalRef: "l-stuck", CreatedAt: old}

	rows, orphans, err := svc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rows != 1 || orphans != 1 {
		t.Fatalf("released rows=%d orphans=%d, want 1 and 1", rows, orphans)
	}
	if want := []int64{1}; !reflect.DeepEqual(repo.deleted, want) {
		t.Fatalf("deleted rows = %v, want %v", repo.deleted, want)
	}
	var deletes []string
	for _, s := range f.seen {
		if strings.HasPrefix(s, "DELETE ") {
			deletes = append(deletes, strings.TrimSpace(s))
		}
	}
	want := []string{"DELETE /v1/leases/l-old-unused", "DELETE /v1/leases/l-stuck", "DELETE /v1/leases/l-orphan-old"}
	if !reflect.DeepEqual(deletes, want) {
		t.Fatalf("provider deletes = %v, want %v", deletes, want)
	}
}

func TestProxyPoolReconcileWithoutProviderConfigIsANoOp(t *testing.T) {
	svc, _, repo := newPoolService(t, nil)
	repo.rows[1] = &Proxy{ID: 1, ManagedBy: ProxyManagedByPool, ExternalRef: "l-1", CreatedAt: time.Unix(0, 0)}
	if rows, orphans, err := svc.Reconcile(context.Background()); err != nil || rows != 0 || orphans != 0 {
		t.Fatalf("Reconcile = %d, %d, %v", rows, orphans, err)
	}
	if len(repo.deleted) != 0 {
		t.Fatal("without a provider nothing may be deleted: the lease could not be released")
	}
}
