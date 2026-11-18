//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

type merge15871Repo struct {
	AccountRepository
	account *Account
}

func (r *merge15871Repo) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account != nil && r.account.ID == id {
		return r.account, nil
	}
	return nil, fmt.Errorf("test account not found")
}

func (r *merge15871Repo) UpdateExtra(context.Context, int64, map[string]any) error {
	return nil
}

type merge15871Logs struct{ UsageLogRepository }

func (*merge15871Logs) GetAccountWindowStats(context.Context, int64, time.Time) (*usagestats.AccountStats, error) {
	return &usagestats.AccountStats{Requests: 7, Tokens: 123}, nil
}

func TestMerge15871Projection(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	observed := now.Add(-time.Hour)
	usage := &UsageInfo{FiveHour: &UsageProgress{Utilization: 90}}
	applyExtraToUsage(usage, map[string]any{
		"codex_5h_used_percent":   nil,
		"codex_7d_used_percent":   49,
		"codex_7d_window_minutes": 43200,
		"codex_usage_updated_at":  observed.Format(time.RFC3339),
	}, now)
	require.Nil(t, usage.FiveHour, "nil deletion must clear the stale in-memory window")
	require.NotNil(t, usage.SevenDay)
	require.Equal(t, 49.0, usage.SevenDay.Utilization)
	require.Equal(t, 43200, usage.SevenDay.WindowMinutes)
	require.NotNil(t, usage.UpdatedAt)
	require.True(t, usage.UpdatedAt.Equal(observed))
	applyExtraToUsage(usage, nil, now)
	require.Nil(t, usage.FiveHour)
	require.Nil(t, usage.SevenDay)
	require.Nil(t, usage.UpdatedAt, "unknown observation must not become read time")
}

func TestMerge15871FreeQuota(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		success    bool
	}{
		{"observed", `{"rate_limit":{"primary_window":{"used_percent":49,"limit_window_seconds":2592000,"reset_after_seconds":2349161},"secondary_window":null}}`, true},
		{"empty", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			account := &Account{
				ID: 1587101, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				Credentials: map[string]any{"plan_type": "free", "access_token": "test-only", "chatgpt_account_id": "test-only-account", "expires_at": "2099-01-01T00:00:00Z"},
				Extra:       map[string]any{"codex_7d_used_percent": 100.0, "codex_usage_updated_at": old.Format(time.RFC3339)},
			}
			repo := &merge15871Repo{account: account}
			var usageCalls, creditCalls, unexpectedCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "GET /backend-api/wham/usage":
					usageCalls.Add(1)
					_, _ = w.Write([]byte(tc.body))
				case "GET /backend-api/wham/rate-limit-reset-credits":
					creditCalls.Add(1)
					_, _ = w.Write([]byte(`{"credits":[]}`))
				default:
					unexpectedCalls.Add(1)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			target, err := url.Parse(srv.URL)
			require.NoError(t, err)
			factory := func(string) (*req.Client, error) {
				return req.C().WrapRoundTripFunc(func(rt req.RoundTripper) req.RoundTripFunc {
					return func(r *req.Request) (*req.Response, error) {
						r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
						return rt.RoundTrip(r)
					}
				}), nil
			}
			svc := &AccountUsageService{
				accountRepo: repo, usageLogRepo: &merge15871Logs{},
				openAIQuotaService: NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, nil, nil), factory, nil),
			}
			usage, err := svc.getOpenAIUsage(context.Background(), account, true)
			require.NoError(t, err)
			require.Zero(t, unexpectedCalls.Load(), "no other method or endpoint may be called")
			require.EqualValues(t, 1, usageCalls.Load(), "both cases must reach the quota usage endpoint")
			require.EqualValues(t, 1, creditCalls.Load(), "the existing read-only credit-details query must be preserved")
			require.NotNil(t, usage.UpdatedAt)
			require.NotNil(t, usage.SevenDay)
			if tc.success {
				require.Equal(t, "active", usage.Source)
				require.Empty(t, usage.ErrorCode)
				require.Empty(t, usage.Error)
				require.Equal(t, 49.0, usage.SevenDay.Utilization)
				require.Equal(t, 43200, usage.SevenDay.WindowMinutes)
				stamp, err := time.Parse(time.RFC3339, account.Extra["codex_usage_updated_at"].(string))
				require.NoError(t, err)
				require.True(t, usage.UpdatedAt.Equal(stamp))
				require.NotNil(t, usage.FiveHour)
				require.True(t, usage.FiveHour.QuotaWindowAbsent)
				require.True(t, usage.FiveHour.UtilizationUnknown)
				require.NotNil(t, usage.FiveHour.WindowStats)
				require.EqualValues(t, 7, usage.FiveHour.WindowStats.Requests)
			} else {
				require.Equal(t, "passive", usage.Source)
				require.Equal(t, "quota_refresh_failed", usage.ErrorCode)
				require.NotEmpty(t, usage.Error)
				require.True(t, usage.UpdatedAt.Equal(old))
				require.Equal(t, 100.0, usage.SevenDay.Utilization)
				require.NotNil(t, usage.FiveHour)
				require.True(t, usage.FiveHour.UtilizationUnknown)
				require.False(t, usage.FiveHour.QuotaWindowAbsent)
			}
		})
	}
}

type merge15871Cache struct {
	SchedulerCache
	values    []Account
	hit       bool
	published []Account
}

func (c *merge15871Cache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	if !c.hit {
		return nil, false, nil
	}
	out := make([]*Account, len(c.values))
	for i := range c.values {
		out[i] = &c.values[i]
	}
	return out, true, nil
}

func (*merge15871Cache) CaptureBucketWriteToken(_ context.Context, b SchedulerBucket) (SchedulerBucketWriteToken, error) {
	return SchedulerBucketWriteToken{Bucket: b, Epoch: 1}, nil
}

func (c *merge15871Cache) SetSnapshot(_ context.Context, _ SchedulerBucket, _ SchedulerBucketWriteToken, a []Account) error {
	c.published = append([]Account(nil), a...)
	return nil
}

type merge15871Values struct{ *merge15871Cache }

func (c *merge15871Values) GetSnapshotValues(context.Context, SchedulerBucket) ([]Account, bool, error) {
	return c.values, true, nil
}

type merge15871Candidates struct {
	AccountRepository
	values []Account
}

func (r *merge15871Candidates) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error) {
	return r.values, nil
}

func TestMerge15871Scheduler(t *testing.T) {
	for _, path := range []string{"values", "pointers", "fallback"} {
		t.Run(path, func(t *testing.T) {
			id := int64(42)
			future := time.Now().Add(time.Hour)
			values := []Account{
				{ID: 1, Status: StatusActive, Schedulable: true},
				{ID: 2, Status: StatusActive, Schedulable: true, ProxyID: &id, Proxy: &Proxy{ID: id, Status: StatusActive}},
				{ID: 3, Status: StatusActive, Schedulable: true, ProxyID: &id},
				{ID: 4, Status: StatusActive, Schedulable: true, ProxyID: &id, Proxy: &Proxy{ID: id, Status: "inactive"}},
				{ID: 5, Status: StatusActive, Schedulable: true, RateLimitResetAt: &future},
			}
			c := &merge15871Cache{values: values, hit: path != "fallback"}
			var cache SchedulerCache = c
			if path == "values" {
				cache = &merge15871Values{c}
			}
			svc := NewSchedulerSnapshotService(cache, nil, &merge15871Candidates{values: values}, nil, nil)
			accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
			require.NoError(t, err)
			ids := make([]int64, len(accounts))
			for i := range accounts {
				ids[i] = accounts[i].ID
			}
			require.Equal(t, []int64{1, 2}, ids)
			if path == "values" {
				require.Same(t, &c.values[0], &accounts[0], "keep the upstream no-copy path")
			}
			if path == "fallback" {
				require.Len(t, c.published, 5, "publish the configured pool, not the filtered result")
			}
		})
	}
}
