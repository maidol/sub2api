//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetOpenAIUsage_FreeOAuth_ForceRefreshDropsStaleFiveHourInSameResponse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	free := &Account{
		ID: 302, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{
			"access_token": "free-access-token", "chatgpt_account_id": "org-free", "plan_type": "free",
		},
		Extra: map[string]any{
			"codex_5h_used_percent": 33, "codex_5h_reset_after_seconds": 9000,
			"codex_5h_window_minutes": 300, "codex_5h_reset_at": "2026-09-12T20:00:00+08:00",
			"codex_7d_used_percent": 49, "codex_7d_window_minutes": 43200,
			"codex_usage_updated_at": "2026-09-12T07:25:45Z",
		},
	}
	repo := &phantomFiveHourAccountRepo{accounts: map[int64]*Account{302: free}}
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
	v, ok := free.Extra["codex_5h_used_percent"]
	require.True(t, ok)
	require.Nil(t, v)
	require.NotNil(t, usage.FiveHour)
	require.True(t, usage.FiveHour.QuotaWindowAbsent)
	require.Zero(t, usage.FiveHour.Utilization)
	require.Nil(t, usage.FiveHour.ResetsAt)
	require.Zero(t, usage.FiveHour.WindowMinutes)
	require.NotNil(t, usage.FiveHour.WindowStats)
	require.NotNil(t, usage.SevenDay)
}
