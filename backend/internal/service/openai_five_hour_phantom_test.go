//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestBuildCodexUsageProgressFromExtra_NilMarkerIsNotAWindow(t *testing.T) {
	now := time.Date(2026, 9, 12, 7, 25, 45, 0, time.UTC)
	extra := map[string]any{
		"codex_5h_used_percent":        nil,
		"codex_5h_reset_after_seconds": nil,
		"codex_5h_window_minutes":      nil,
		"codex_5h_reset_at":            nil,
		"codex_7d_used_percent":        49,
		"codex_7d_window_minutes":      43200,
		"codex_usage_updated_at":       "2026-09-12T07:25:45Z",
	}

	if progress := buildCodexUsageProgressFromExtra(extra, "5h", now); progress != nil {
		t.Fatalf("5h progress = %+v, want nil", progress)
	}
	if progress := buildCodexUsageProgressFromExtra(extra, "7d", now); progress == nil {
		t.Fatal("7d progress = nil, want non-nil")
	}
}

type phantomFiveHourAccountRepo struct {
	AccountRepository
	accounts map[int64]*Account
}

func (r *phantomFiveHourAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if acc, ok := r.accounts[id]; ok {
		return acc, nil
	}
	return nil, context.Canceled
}

func (r *phantomFiveHourAccountRepo) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	return nil
}

type phantomFiveHourUsageLogRepo struct{ UsageLogRepository }

func (r *phantomFiveHourUsageLogRepo) GetAccountWindowStats(context.Context, int64, time.Time) (*usagestats.AccountStats, error) {
	return &usagestats.AccountStats{Requests: 7, Tokens: 123}, nil
}

func TestGetOpenAIUsage_FreeOAuth_ExplicitNullSecondaryWindowYieldsNoFiveHour(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	free := &Account{
		ID: 301, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{
			"access_token": "free-access-token", "chatgpt_account_id": "org-free", "plan_type": "free",
		},
	}
	repo := &phantomFiveHourAccountRepo{accounts: map[int64]*Account{301: free}}
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(free): "free-access-token"}}
	tokenProvider := NewOpenAITokenProvider(repo, tokenCache, nil)
	const body = `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":49,"limit_window_seconds":2592000,"reset_after_seconds":2349161},"secondary_window":null}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	quotaService := NewOpenAIQuotaService(repo, nil, tokenProvider, newQuotaRedirectingFactory(srv))
	svc := &AccountUsageService{accountRepo: repo, openAIQuotaService: quotaService, usageLogRepo: &phantomFiveHourUsageLogRepo{}}
	usage, err := svc.getOpenAIUsage(ctx, free, true)
	require.NoError(t, err)
	require.NotNil(t, usage.FiveHour, "local traffic should remain visible")
	require.True(t, usage.FiveHour.QuotaWindowAbsent)
	require.Zero(t, usage.FiveHour.Utilization)
	require.Nil(t, usage.FiveHour.ResetsAt)
	require.Zero(t, usage.FiveHour.WindowMinutes)
	require.NotNil(t, usage.FiveHour.WindowStats)
	require.EqualValues(t, 7, usage.FiveHour.WindowStats.Requests)
	require.EqualValues(t, 123, usage.FiveHour.WindowStats.Tokens)
	require.NotNil(t, usage.SevenDay)
	require.InDelta(t, 49, usage.SevenDay.Utilization, 0.01)
	require.NotNil(t, usage.SevenDay.WindowStats)
	require.EqualValues(t, 7, usage.SevenDay.WindowStats.Requests)
}
