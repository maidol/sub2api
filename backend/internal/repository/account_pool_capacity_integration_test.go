//go:build integration

package repository

import (
	"context"
	"errors"
	"sync"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newPoolSlot(t *testing.T, repo *proxyRepository, ref string, shareable bool, max int) *service.Proxy {
	t.Helper()
	p := &service.Proxy{Name: "slot-" + ref, Protocol: "http", Host: "vpngate", Port: 20001, Status: service.StatusActive,
		FallbackMode: service.FallbackModeNone, ExpiryWarnDays: 7, ManagedBy: service.ProxyManagedByPool,
		ExternalRef: ref, PoolShareable: shareable, PoolShareMax: max}
	require.NoError(t, repo.Create(context.Background(), p))
	return p
}

func newCapacityAccount(name string, proxyID *int64) *service.Account {
	return &service.Account{Name: name, Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Concurrency: 3, Priority: 50, Schedulable: true,
		Credentials: map[string]any{}, Extra: map[string]any{}, ProxyID: proxyID}
}

func TestPoolCapacityRejectsASecondAccountOnAnUnsharedSlot(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	ctx = dbent.NewTxContext(ctx, tx)
	proxies := newProxyRepositoryWithSQL(tx.Client(), tx)
	accounts := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	slot := newPoolSlot(t, proxies, "l-solo", false, 0)

	require.NoError(t, accounts.Create(ctx, newCapacityAccount("first", &slot.ID)))
	err := accounts.Create(ctx, newCapacityAccount("second", &slot.ID))
	require.ErrorIs(t, err, service.ErrProxyPoolSlotNotShared)
}

func TestPoolCapacityHonoursTheShareLimitOnCreateAndUpdate(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	ctx = dbent.NewTxContext(ctx, tx)
	proxies := newProxyRepositoryWithSQL(tx.Client(), tx)
	accounts := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	slot := newPoolSlot(t, proxies, "l-two", true, 2)

	require.NoError(t, accounts.Create(ctx, newCapacityAccount("a", &slot.ID)))
	require.NoError(t, accounts.Create(ctx, newCapacityAccount("b", &slot.ID)))
	err := accounts.Create(ctx, newCapacityAccount("c", &slot.ID))
	require.True(t, errors.Is(err, service.ErrProxyPoolSlotFull), "got %v", err)

	c := newCapacityAccount("c", nil)
	require.NoError(t, accounts.Create(ctx, c))
	c.ProxyID = &slot.ID
	require.ErrorIs(t, accounts.Update(ctx, c), service.ErrProxyPoolSlotFull)
}

func TestPoolCapacityUnlimitedAndLoweredLimit(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	ctx = dbent.NewTxContext(ctx, tx)
	proxies := newProxyRepositoryWithSQL(tx.Client(), tx)
	accounts := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	slot := newPoolSlot(t, proxies, "l-open", true, 0)

	created := make([]*service.Account, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		a := newCapacityAccount(name, &slot.ID)
		require.NoError(t, accounts.Create(ctx, a), "max 0 means no limit")
		created = append(created, a)
	}

	slot.PoolShareMax = 1
	require.NoError(t, proxies.Update(ctx, slot))
	created[0].Name = "a-renamed"
	require.NoError(t, accounts.Update(ctx, created[0]), "an account already on the slot keeps saving")
}

func TestPoolCapacityIgnoresShadowsAndHandMadeProxies(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	ctx = dbent.NewTxContext(ctx, tx)
	proxies := newProxyRepositoryWithSQL(tx.Client(), tx)
	accounts := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	slot := newPoolSlot(t, proxies, "l-shadow", false, 0)
	plain := mustCreateProxy(t, tx.Client(), &service.Proxy{Name: "hand-made"})

	parent := newCapacityAccount("parent", &slot.ID)
	require.NoError(t, accounts.Create(ctx, parent))
	shadow := newCapacityAccount("shadow", nil)
	require.NoError(t, accounts.Create(ctx, shadow))
	_, err := tx.Client().ExecContext(ctx, "UPDATE accounts SET parent_account_id = $1, quota_dimension = 'spark' WHERE id = $2", parent.ID, shadow.ID)
	require.NoError(t, err)
	shadow.ParentAccountID = &parent.ID
	shadow.QuotaDimension = service.QuotaDimensionSpark
	shadow.ProxyID = &slot.ID
	require.NoError(t, accounts.Update(ctx, shadow), "a shadow follows its parent onto an unshared slot")

	for _, name := range []string{"x", "y"} {
		require.NoError(t, accounts.Create(ctx, newCapacityAccount(name, &plain.ID)))
	}
}

// Two transactions race for the last place on a shared slot: the row lock
// makes the second one count the first one's account.
func TestPoolCapacityConcurrentSavesTakeTheLastPlaceOnce(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	proxies := newProxyRepositoryWithSQL(client, integrationDB)
	accounts := newAccountRepositoryWithSQL(client, integrationDB, nil)
	slot := newPoolSlot(t, proxies, "l-race", true, 1)
	// These writes are committed: remove the outbox events Create enqueued
	// too, or suites that count scheduler_outbox see them.
	t.Cleanup(func() {
		_, _ = integrationDB.Exec("DELETE FROM scheduler_outbox WHERE account_id IN (SELECT id FROM accounts WHERE proxy_id = $1)", slot.ID)
		_, _ = integrationDB.Exec("DELETE FROM accounts WHERE proxy_id = $1", slot.ID)
		_, _ = integrationDB.Exec("DELETE FROM proxies WHERE id = $1", slot.ID)
	})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = accounts.Create(ctx, newCapacityAccount("race", &slot.ID))
		}(i)
	}
	wg.Wait()
	ok, full := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, service.ErrProxyPoolSlotFull):
			full++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, ok)
	require.Equal(t, 1, full)
}
