package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestGetAccountModelFamilyWindowStats(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	start := time.Date(2026, 9, 24, 7, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Hour)

	mock.ExpectQuery(`FROM usage_logs\s+WHERE account_id = \$1 AND created_at >= \$2 AND created_at < \$3\s+AND LOWER\(COALESCE\(NULLIF\(upstream_model, ''\), model\)\) LIKE ANY\(\$4\)`).
		WithArgs(int64(148), start, end, pq.Array([]string{"claude%", "gpt%"})).
		WillReturnRows(sqlmock.NewRows([]string{"requests", "tokens", "cost", "standard_cost", "user_cost"}).
			AddRow(int64(3), int64(4200), 0.5, 0.4, 0.6))

	stats, err := repo.GetAccountModelFamilyWindowStats(context.Background(), 148, start, end, []string{"Claude", "gpt"})
	require.NoError(t, err)
	require.Equal(t, int64(3), stats.Requests)
	require.Equal(t, int64(4200), stats.Tokens)
	require.InDelta(t, 0.6, stats.UserCost, 1e-9)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetAccountModelFamilyWindowStatsExcludesArgon(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	start := time.Date(2026, 11, 15, 7, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Hour)
	mock.ExpectQuery(`AND NOT \(LOWER\(COALESCE\(NULLIF\(upstream_model, ''\), model\)\) LIKE ANY\(\$5\)\)`).
		WithArgs(int64(7), start, end, pq.Array([]string{"gemini%"}), pq.Array([]string{"gemini-4-argon%"})).
		WillReturnRows(sqlmock.NewRows([]string{"requests", "tokens", "cost", "standard_cost", "user_cost"}).AddRow(3, 4200, 0.5, 0.4, 0.6))
	stats, err := repo.GetAccountModelFamilyWindowStats(t.Context(), 7, start, end, []string{"Gemini"}, "Gemini-4-Argon")
	require.NoError(t, err)
	require.Equal(t, int64(3), stats.Requests)
	require.InDelta(t, 0.6, stats.UserCost, 1e-9)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetAccountModelFamilyWindowStatsNoPrefixes(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	stats, err := repo.GetAccountModelFamilyWindowStats(context.Background(), 1, time.Now(), time.Now(), nil)
	require.NoError(t, err)
	require.Zero(t, stats.Requests)
	require.NoError(t, mock.ExpectationsWereMet())
}
