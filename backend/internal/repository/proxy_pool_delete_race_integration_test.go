//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func waitForProxyRowLockWait(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := integrationDB.QueryRowContext(context.Background(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock'
				  AND query LIKE '%SELECT id FROM proxies WHERE id = $1 AND deleted_at IS NULL FOR UPDATE%'
			)`).Scan(&waiting)
		require.NoError(t, err)
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("proxy delete query did not wait for the save transaction's row lock")
}

func TestPoolProxyDeleteAndAccountSaveSerialize(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	proxies := newProxyRepositoryWithSQL(client, integrationDB)
	accounts := newAccountRepositoryWithSQL(client, integrationDB, nil)

	first := newPoolSlot(t, proxies, "race-save-first", true, 0)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec("DELETE FROM scheduler_outbox WHERE account_id IN (SELECT id FROM accounts WHERE proxy_id = $1)", first.ID)
		_, _ = integrationDB.Exec("DELETE FROM accounts WHERE proxy_id = $1", first.ID)
		_, _ = integrationDB.Exec("DELETE FROM proxies WHERE id = $1", first.ID)
	})

	// Save first in a transaction and retain the proxy row lock until commit.
	saveTx, err := client.Tx(ctx)
	require.NoError(t, err)
	saveCtx := dbent.NewTxContext(ctx, saveTx)
	account := newCapacityAccount("race-save", &first.ID)
	require.NoError(t, accounts.Create(saveCtx, account))

	deleteStarted := make(chan struct{})
	deleteDone := make(chan struct {
		deleted bool
		err     error
	}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(deleteStarted)
		deleted, err := proxies.DeletePoolProxyIfUnused(ctx, first.ID)
		deleteDone <- struct {
			deleted bool
			err     error
		}{deleted: deleted, err: err}
	}()
	<-deleteStarted
	waitForProxyRowLockWait(t)

	require.NoError(t, saveTx.Commit())
	result := <-deleteDone
	wg.Wait()
	require.NoError(t, result.err)
	require.False(t, result.deleted, "committed account must keep the proxy live")

	second := newPoolSlot(t, proxies, "race-delete-first", true, 0)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec("DELETE FROM proxies WHERE id = $1", second.ID)
	})
	deleted, err := proxies.DeletePoolProxyIfUnused(ctx, second.ID)
	require.NoError(t, err)
	require.True(t, deleted)
	newAccount := newCapacityAccount("race-after-delete", &second.ID)
	require.ErrorIs(t, accounts.Create(ctx, newAccount), service.ErrProxyNotFound)
}
