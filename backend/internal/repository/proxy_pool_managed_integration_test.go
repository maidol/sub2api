//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Migration 241 and the ent fields must agree: a pool-managed proxy keeps its
// marker and lease ID through create, read, list and an admin update.
func TestProxyPoolManagedFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newProxyRepositoryWithSQL(tx.Client(), tx)

	managed := &service.Proxy{
		Name: "Proxy pool · slot01", Protocol: "http", Host: "vpngate", Port: 20001,
		Username: "slot01", Password: "0123456789abcdef0123456789abcdef", Status: service.StatusActive,
		FallbackMode: service.FallbackModeNone, ExpiryWarnDays: 7,
		ManagedBy: service.ProxyManagedByPool, ExternalRef: "l-abc",
	}
	require.NoError(t, repo.Create(ctx, managed))
	plain := &service.Proxy{
		Name: "hand-made", Protocol: "http", Host: "127.0.0.1", Port: 8080,
		Status: service.StatusActive, FallbackMode: service.FallbackModeNone, ExpiryWarnDays: 7,
	}
	require.NoError(t, repo.Create(ctx, plain))

	got, err := repo.GetByID(ctx, managed.ID)
	require.NoError(t, err)
	require.Equal(t, service.ProxyManagedByPool, got.ManagedBy)
	require.Equal(t, "l-abc", got.ExternalRef)

	gotPlain, err := repo.GetByID(ctx, plain.ID)
	require.NoError(t, err)
	require.Equal(t, "", gotPlain.ManagedBy, "existing and hand-made rows default to empty")
	require.Equal(t, "", gotPlain.ExternalRef)

	// An admin edit goes through Update, which does not write the marker; it
	// must survive.
	got.Name = "renamed"
	require.NoError(t, repo.Update(ctx, got))
	again, err := repo.GetByID(ctx, managed.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", again.Name)
	require.Equal(t, service.ProxyManagedByPool, again.ManagedBy)
	require.Equal(t, "l-abc", again.ExternalRef)

	all, err := repo.ListAllForFallback(ctx)
	require.NoError(t, err)
	found := false
	for _, p := range all {
		if p.ID == managed.ID {
			found = true
			require.Equal(t, "l-abc", p.ExternalRef)
		}
	}
	require.True(t, found, "reconcile reads managed rows through ListAllForFallback")
}

// Migration 242: share settings default to "not shared, no limit" and survive
// create, read and update.
func TestProxyPoolShareFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newProxyRepositoryWithSQL(tx.Client(), tx)

	p := &service.Proxy{
		Name: "Proxy pool · slot01", Protocol: "http", Host: "vpngate", Port: 20001,
		Username: "slot01", Password: "0123456789abcdef0123456789abcdef", Status: service.StatusActive,
		FallbackMode: service.FallbackModeNone, ExpiryWarnDays: 7,
		ManagedBy: service.ProxyManagedByPool, ExternalRef: "l-share",
	}
	require.NoError(t, repo.Create(ctx, p))
	got, err := repo.GetByID(ctx, p.ID)
	require.NoError(t, err)
	require.False(t, got.PoolShareable)
	require.Equal(t, 0, got.PoolShareMax)

	got.PoolShareable, got.PoolShareMax = true, 5
	require.NoError(t, repo.Update(ctx, got))
	again, err := repo.GetByID(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, again.PoolShareable)
	require.Equal(t, 5, again.PoolShareMax)
}

// Occupancy counts live accounts on pool-managed proxies only, and leaves
// spark shadows out: a shadow always rides on its parent's proxy.
func TestProxyPoolOccupancyExcludesShadowsAndDeleted(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newProxyRepositoryWithSQL(client, tx)

	pool := &service.Proxy{Name: "slot01", Protocol: "http", Host: "vpngate", Port: 20001, Status: service.StatusActive,
		FallbackMode: service.FallbackModeNone, ExpiryWarnDays: 7, ManagedBy: service.ProxyManagedByPool, ExternalRef: "l-occ"}
	require.NoError(t, repo.Create(ctx, pool))
	plain := mustCreateProxy(t, client, &service.Proxy{Name: "hand-made"})

	parent := mustCreateAccount(t, client, &service.Account{Name: "parent", ProxyID: &pool.ID})
	mustCreateAccount(t, client, &service.Account{Name: "second", ProxyID: &pool.ID})
	shadow := mustCreateAccount(t, client, &service.Account{Name: "shadow", ProxyID: &pool.ID})
	_, err := client.ExecContext(ctx, "UPDATE accounts SET parent_account_id = $1, quota_dimension = 'spark' WHERE id = $2", parent.ID, shadow.ID)
	require.NoError(t, err)
	gone := mustCreateAccount(t, client, &service.Account{Name: "gone", ProxyID: &pool.ID})
	_, err = client.ExecContext(ctx, "UPDATE accounts SET deleted_at = NOW() WHERE id = $1", gone.ID)
	require.NoError(t, err)
	mustCreateAccount(t, client, &service.Account{Name: "plain", ProxyID: &plain.ID})

	occ, err := repo.GetPoolOccupancy(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), occ[pool.ID])
	_, hasPlain := occ[plain.ID]
	require.False(t, hasPlain, "hand-made proxies are not in the occupancy map")
}
