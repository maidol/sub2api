//go:build unit

package repository

import (
	"context"
	"regexp"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newCapacityGuardTestClient(t *testing.T) (*dbent.Client, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() {
		_ = client.Close()
		_ = db.Close()
	})
	return client, mock
}

func TestPoolCapacityRejectsMissingProxy(t *testing.T) {
	client, mock := newCapacityGuardTestClient(t)
	proxyID := int64(42)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT managed_by, pool_shareable, pool_share_max FROM proxies WHERE id = $1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(proxyID).
		WillReturnRows(sqlmock.NewRows([]string{"managed_by", "pool_shareable", "pool_share_max"}))

	err := guardPoolProxyCapacity(context.Background(), client, &service.Account{ProxyID: &proxyID})
	require.ErrorIs(t, err, service.ErrProxyNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPoolCapacityAllowsUnchangedProxyAndShadow(t *testing.T) {
	t.Run("unchanged proxy", func(t *testing.T) {
		client, mock := newCapacityGuardTestClient(t)
		proxyID := int64(42)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT proxy_id FROM accounts WHERE id = $1")).
			WithArgs(int64(7)).
			WillReturnRows(sqlmock.NewRows([]string{"proxy_id"}).AddRow(proxyID))

		err := guardPoolProxyCapacity(context.Background(), client, &service.Account{ID: 7, ProxyID: &proxyID})
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("shadow", func(t *testing.T) {
		client, mock := newCapacityGuardTestClient(t)
		proxyID, parentID := int64(42), int64(9)
		err := guardPoolProxyCapacity(context.Background(), client, &service.Account{ProxyID: &proxyID, ParentAccountID: &parentID})
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
