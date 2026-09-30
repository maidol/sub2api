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
