//go:build unit

package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestProxyPoolDeleteIfUnused(t *testing.T) {
	lockedProxyQuery := regexp.QuoteMeta("SELECT id FROM proxies WHERE id = $1 AND deleted_at IS NULL FOR UPDATE")
	accountCountQuery := regexp.QuoteMeta("SELECT COUNT(*) FROM accounts WHERE proxy_id = $1 AND deleted_at IS NULL")
	backupRefQuery := regexp.QuoteMeta("SELECT 1 FROM proxies WHERE backup_proxy_id = $1 AND deleted_at IS NULL LIMIT 1")

	t.Run("missing proxy", func(t *testing.T) {
		client, mock := newCapacityGuardTestClient(t)
		repo := newProxyRepositoryWithSQL(client, nil)
		mock.ExpectBegin()
		mock.ExpectQuery(lockedProxyQuery).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"id"}))
		mock.ExpectCommit()

		deleted, err := repo.DeletePoolProxyIfUnused(context.Background(), 42)
		require.NoError(t, err)
		require.False(t, deleted)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("account still uses proxy", func(t *testing.T) {
		client, mock := newCapacityGuardTestClient(t)
		repo := newProxyRepositoryWithSQL(client, nil)
		mock.ExpectBegin()
		mock.ExpectQuery(lockedProxyQuery).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))
		mock.ExpectQuery(accountCountQuery).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
		mock.ExpectCommit()

		deleted, err := repo.DeletePoolProxyIfUnused(context.Background(), 42)
		require.NoError(t, err)
		require.False(t, deleted)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("proxy is a live backup", func(t *testing.T) {
		client, mock := newCapacityGuardTestClient(t)
		repo := newProxyRepositoryWithSQL(client, nil)
		mock.ExpectBegin()
		mock.ExpectQuery(lockedProxyQuery).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))
		mock.ExpectQuery(accountCountQuery).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
		mock.ExpectQuery(backupRefQuery).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
		mock.ExpectCommit()

		deleted, err := repo.DeletePoolProxyIfUnused(context.Background(), 42)
		require.NoError(t, err)
		require.False(t, deleted)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("unused proxy is soft deleted in transaction", func(t *testing.T) {
		client, mock := newCapacityGuardTestClient(t)
		repo := newProxyRepositoryWithSQL(client, nil)
		mock.ExpectBegin()
		mock.ExpectQuery(lockedProxyQuery).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))
		mock.ExpectQuery(accountCountQuery).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
		mock.ExpectQuery(backupRefQuery).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
		mock.ExpectExec(`UPDATE "proxies" SET "updated_at" = \$1, "deleted_at" = \$2 WHERE "proxies"\."id" = \$3 AND "proxies"\."deleted_at" IS NULL`).
			WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), int64(42)).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		deleted, err := repo.DeletePoolProxyIfUnused(context.Background(), 42)
		require.NoError(t, err)
		require.True(t, deleted)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("query failure rolls transaction back", func(t *testing.T) {
		client, mock := newCapacityGuardTestClient(t)
		repo := newProxyRepositoryWithSQL(client, nil)
		queryErr := errors.New("database unavailable")
		mock.ExpectBegin()
		mock.ExpectQuery(lockedProxyQuery).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))
		mock.ExpectQuery(accountCountQuery).WithArgs(int64(42)).WillReturnError(queryErr)
		mock.ExpectRollback()

		deleted, err := repo.DeletePoolProxyIfUnused(context.Background(), 42)
		require.ErrorIs(t, err, queryErr)
		require.False(t, deleted)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
