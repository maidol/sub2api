//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGetOpenAIUsage_ExistingFiveHourWindowGetsLocalStats(t *testing.T) {
	t.Parallel()
	acc := &Account{
		ID: 303, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"access_token": "tok", "chatgpt_account_id": "org", "plan_type": "plus"},
		Extra: map[string]any{
			"codex_5h_used_percent": 12, "codex_5h_window_minutes": 300,
			"codex_5h_reset_at": "2099-01-01T00:00:00Z", "codex_7d_used_percent": 49,
			"codex_7d_window_minutes": 43200, "codex_usage_updated_at": time.Now().UTC().Format(time.RFC3339),
		},
	}
	repo := &phantomFiveHourAccountRepo{accounts: map[int64]*Account{303: acc}}
	svc := &AccountUsageService{accountRepo: repo, usageLogRepo: &phantomFiveHourUsageLogRepo{}}
	usage, err := svc.getOpenAIUsage(context.Background(), acc, false)
	require.NoError(t, err)
	require.NotNil(t, usage.FiveHour)
	require.NotNil(t, usage.FiveHour.WindowStats)
	require.EqualValues(t, 7, usage.FiveHour.WindowStats.Requests)
}

func TestBuildCodexUsageProgressFromExtra_GenuineZeroPercentIsStillAWindow(t *testing.T) {
	now := time.Date(2026, 9, 12, 7, 25, 45, 0, time.UTC)
	extra := map[string]any{
		"codex_5h_used_percent": 0, "codex_5h_reset_after_seconds": 9000,
		"codex_5h_window_minutes": 300, "codex_usage_updated_at": "2026-09-12T07:25:45Z",
	}
	progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
	require.NotNil(t, progress)
	require.EqualValues(t, 0, progress.Utilization)
	require.EqualValues(t, 300, progress.WindowMinutes)
}
