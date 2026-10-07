package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type proxyGetterFunc func(context.Context, int64) (*Proxy, error)

func (f proxyGetterFunc) GetByID(ctx context.Context, id int64) (*Proxy, error) {
	return f(ctx, id)
}

func TestProxyUnavailableReason(t *testing.T) {
	now := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	id := int64(42)
	future := now.Add(time.Minute)
	past := now.Add(-time.Minute)

	tests := []struct {
		name    string
		proxyID *int64
		proxy   *Proxy
		want    string
	}{
		{
			name: "unbound account is available",
		},
		{
			name:    "missing bound proxy is deleted",
			proxyID: &id,
			want:    "deleted",
		},
		{
			name:    "inactive proxy is unavailable",
			proxyID: &id,
			proxy:   &Proxy{ID: id, Status: "inactive"},
			want:    "inactive",
		},
		{
			name:    "mismatched loaded proxy is unavailable",
			proxyID: &id,
			proxy:   &Proxy{ID: id + 1, Status: StatusActive, ExpiresAt: &future},
			want:    "mismatch",
		},
		{
			name:    "active unexpired proxy is available",
			proxyID: &id,
			proxy:   &Proxy{ID: id, Status: StatusActive, ExpiresAt: &future},
		},
		{
			name:    "proxy expiring now is expired",
			proxyID: &id,
			proxy:   &Proxy{ID: id, Status: StatusActive, ExpiresAt: &now},
			want:    "expired",
		},
		{
			name:    "expired proxy is unavailable",
			proxyID: &id,
			proxy:   &Proxy{ID: id, Status: StatusActive, ExpiresAt: &past},
			want:    "expired",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := proxyUnavailableReason(tt.proxyID, tt.proxy, now); got != tt.want {
				t.Fatalf("proxyUnavailableReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

type proxyGateSnapshotCache struct {
	SchedulerCache
	accounts        []*Account
	setAccountCalls []*Account
}

func (c *proxyGateSnapshotCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	return c.accounts, true, nil
}

func (c *proxyGateSnapshotCache) SetAccount(_ context.Context, account *Account) error {
	c.setAccountCalls = append(c.setAccountCalls, account)
	return nil
}

type proxyUsabilityAccountRepo struct {
	AccountRepository
	account *Account
	err     error
}

func (r *proxyUsabilityAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, r.err
}

func TestSchedulerSnapshotFiltersAccountsWithUnavailableProxies(t *testing.T) {
	now := time.Now()
	proxyID := int64(42)
	active := &Proxy{ID: proxyID, Status: StatusActive}
	inactive := &Proxy{ID: proxyID, Status: "inactive"}
	expiredAt := now.Add(-time.Minute)
	expired := &Proxy{ID: proxyID, Status: StatusActive, ExpiresAt: &expiredAt}
	cache := &proxyGateSnapshotCache{accounts: []*Account{
		{ID: 1},
		{ID: 2, ProxyID: &proxyID, Proxy: active},
		{ID: 3, ProxyID: &proxyID, Proxy: inactive},
		{ID: 4, ProxyID: &proxyID},
		{ID: 5, ProxyID: &proxyID, Proxy: expired},
	}}
	svc := NewSchedulerSnapshotService(cache, nil, nil, nil, nil)

	accounts, _, err := svc.ListSchedulableAccounts(t.Context(), nil, PlatformOpenAI, false)
	if err != nil {
		t.Fatalf("ListSchedulableAccounts() error = %v", err)
	}
	if len(accounts) != 2 || accounts[0].ID != 1 || accounts[1].ID != 2 {
		t.Fatalf("ListSchedulableAccounts() IDs = %v, want [1 2]", proxyUsabilityAccountIDs(accounts))
	}
}

func TestUpdateAccountInCacheReloadsMissingBoundProxy(t *testing.T) {
	proxyID := int64(42)
	account := &Account{ID: 7, ProxyID: &proxyID}
	reloaded := &Account{ID: 7, ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Status: StatusActive}}
	cache := &proxyGateSnapshotCache{}
	repo := &proxyUsabilityAccountRepo{account: reloaded}
	svc := NewSchedulerSnapshotService(cache, nil, repo, nil, nil)

	if err := svc.UpdateAccountInCache(t.Context(), account); err != nil {
		t.Fatalf("UpdateAccountInCache() error = %v", err)
	}
	if len(cache.setAccountCalls) != 1 || cache.setAccountCalls[0] != reloaded {
		t.Fatalf("SetAccount calls = %v, want reloaded account", cache.setAccountCalls)
	}
}

func TestUpdateAccountInCacheReloadsMismatchedBoundProxy(t *testing.T) {
	proxyID := int64(42)
	account := &Account{ID: 7, ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID + 1, Status: StatusActive}}
	reloaded := &Account{ID: 7, ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Status: StatusActive}}
	cache := &proxyGateSnapshotCache{}
	repo := &proxyUsabilityAccountRepo{account: reloaded}
	svc := NewSchedulerSnapshotService(cache, nil, repo, nil, nil)

	if err := svc.UpdateAccountInCache(t.Context(), account); err != nil {
		t.Fatalf("UpdateAccountInCache() error = %v", err)
	}
	if len(cache.setAccountCalls) != 1 || cache.setAccountCalls[0] != reloaded {
		t.Fatalf("SetAccount calls = %v, want reloaded account", cache.setAccountCalls)
	}
}

func proxyUsabilityAccountIDs(accounts []Account) []int64 {
	ids := make([]int64, len(accounts))
	for i := range accounts {
		ids[i] = accounts[i].ID
	}
	return ids
}

func TestGrokTestProxyURLFailsClosed(t *testing.T) {
	proxyID := int64(42)
	account := &Account{ProxyID: &proxyID}

	proxyURL, err := (&AccountTestService{}).grokTestProxyURL(t.Context(), account)
	if proxyURL != "" {
		t.Fatalf("grokTestProxyURL() = %q, want empty on error", proxyURL)
	}
	if _, ok := errors.AsType[*UpstreamFailoverError](err); !ok {
		t.Fatalf("grokTestProxyURL() error = %T, want *UpstreamFailoverError", err)
	}
}

func TestResolveAccountProxyURLFailClosed(t *testing.T) {
	id := int64(42)
	dbErr := errors.New("database unavailable")
	activeProxy := &Proxy{ID: id, Protocol: "http", Host: "proxy.example", Port: 8080, Status: StatusActive}

	tests := []struct {
		name       string
		account    *Account
		getter     proxyGetter
		wantURL    string
		wantReason string
	}{
		{
			name:    "unbound account stays direct",
			account: &Account{},
		},
		{
			name:       "preloaded active proxy is used",
			account:    &Account{ProxyID: &id, Proxy: activeProxy},
			wantURL:    "http://proxy.example:8080",
			wantReason: "",
		},
		{
			name:       "missing relation without getter fails closed",
			account:    &Account{ProxyID: &id},
			wantReason: "proxy_unavailable_lookup-failed",
		},
		{
			name:    "not found is classified as deleted",
			account: &Account{ProxyID: &id},
			getter: proxyGetterFunc(func(context.Context, int64) (*Proxy, error) {
				return nil, ErrProxyNotFound
			}),
			wantReason: "proxy_unavailable_deleted",
		},
		{
			name:    "lookup error fails closed",
			account: &Account{ProxyID: &id},
			getter: proxyGetterFunc(func(context.Context, int64) (*Proxy, error) {
				return nil, dbErr
			}),
			wantReason: "proxy_unavailable_lookup-failed",
		},
		{
			name:    "getter returning nil proxy fails closed",
			account: &Account{ProxyID: &id},
			getter: proxyGetterFunc(func(context.Context, int64) (*Proxy, error) {
				return nil, nil
			}),
			wantReason: "proxy_unavailable_deleted",
		},
		{
			name:       "inactive preloaded proxy fails closed",
			account:    &Account{ProxyID: &id, Proxy: &Proxy{ID: id, Status: "inactive"}},
			wantReason: "proxy_unavailable_inactive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, err := resolveAccountProxyURL(t.Context(), tt.getter, tt.account)
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("resolveAccountProxyURL() error = %v", err)
				}
				if gotURL != tt.wantURL {
					t.Fatalf("resolveAccountProxyURL() = %q, want %q", gotURL, tt.wantURL)
				}
				return
			}

			if gotURL != "" {
				t.Fatalf("resolveAccountProxyURL() URL = %q, want empty on error", gotURL)
			}
			failoverErr, ok := errors.AsType[*UpstreamFailoverError](err)
			if !ok {
				t.Fatalf("resolveAccountProxyURL() error = %T, want *UpstreamFailoverError", err)
			}
			if got := string(failoverErr.Reason); got != tt.wantReason {
				t.Fatalf("failover reason = %q, want %q", got, tt.wantReason)
			}
			if !failoverErr.ShouldRetryNextAccount() {
				t.Fatal("proxy unavailability must request account failover")
			}
			if failoverErr.RetryableOnSameAccount {
				t.Fatal("proxy unavailability must not trigger same-account retry/cooldown")
			}
		})
	}
}
