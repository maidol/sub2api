//go:build unit

package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBulkUpdateRejectsMissingProxy(t *testing.T) {
	client, mock := newCapacityGuardTestClient(t)
	repo := newAccountRepositoryWithSQL(client, nil, nil)
	proxyID := int64(42)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM proxies WHERE id = $1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(proxyID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectRollback()

	_, err := repo.BulkUpdate(context.Background(), []int64{7}, service.AccountBulkUpdate{ProxyID: &proxyID})
	require.ErrorIs(t, err, service.ErrProxyNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateLocksExistingProxyBeforeUpdatingAccounts(t *testing.T) {
	client, mock := newCapacityGuardTestClient(t)
	repo := newAccountRepositoryWithSQL(client, nil, nil)
	proxyID := int64(42)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM proxies WHERE id = $1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(proxyID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(proxyID))
	mock.ExpectExec(`(?s)UPDATE accounts SET proxy_id = \$1, extra = CASE.*WHERE id = ANY\(\$2\) AND deleted_at IS NULL`).
		WithArgs(proxyID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	rows, err := repo.BulkUpdate(context.Background(), []int64{7}, service.AccountBulkUpdate{ProxyID: &proxyID})
	require.NoError(t, err)
	require.Zero(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateAllowsProxyClearWithoutProxyLookup(t *testing.T) {
	client, mock := newCapacityGuardTestClient(t)
	repo := newAccountRepositoryWithSQL(client, nil, nil)
	clear := int64(0)

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET proxy_id = NULL, extra = CASE.*updated_at = NOW\(\) WHERE id = ANY\(\$1\) AND deleted_at IS NULL`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	rows, err := repo.BulkUpdate(context.Background(), []int64{7}, service.AccountBulkUpdate{ProxyID: &clear})
	require.NoError(t, err)
	require.Zero(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}
