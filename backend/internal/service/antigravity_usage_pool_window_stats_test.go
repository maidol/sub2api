package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type agPoolStatsCall struct {
	start, end time.Time
	prefixes   []string
}

type agPoolStatsRepoStub struct {
	usageBatchLogRepoStub
	calls []agPoolStatsCall
}

func (r *agPoolStatsRepoStub) GetAccountModelFamilyWindowStats(_ context.Context, _ int64, start, end time.Time, prefixes []string) (*usagestats.AccountStats, error) {
	r.calls = append(r.calls, agPoolStatsCall{start: start, end: end, prefixes: prefixes})
	return &usagestats.AccountStats{Requests: int64(len(r.calls)), Tokens: 1000}, nil
}

func TestWithAntigravityPoolWindowStats(t *testing.T) {
	repo := &agPoolStatsRepoStub{}
	svc := &AccountUsageService{usageLogRepo: repo, cache: NewUsageCache()}

	now := time.Now()
	fiveHourReset := now.Add(4 * time.Hour)
	weeklyReset := now.Add(3 * 24 * time.Hour)
	original := &UsageInfo{AntigravityPools: []AntigravityPoolUsage{
		{
			Pool:     antigravityPoolGemini,
			FiveHour: &UsageProgress{Utilization: 0.3, ResetsAt: &fiveHourReset},
			Weekly:   &UsageProgress{Utilization: 0.1, ResetsAt: &weeklyReset},
		},
		{
			// 上游没下发周窗口：保持 nil，不补一行
			Pool:     antigravityPoolClaudeGPT,
			FiveHour: &UsageProgress{Utilization: 0, ResetsAt: &fiveHourReset},
		},
	}}

	got := svc.withAntigravityPoolWindowStats(context.Background(), &Account{ID: 7}, original)

	require.Len(t, got.AntigravityPools, 2)
	gemini := got.AntigravityPools[0]
	require.NotNil(t, gemini.FiveHour.WindowStats)
	require.NotNil(t, gemini.Weekly.WindowStats)
	require.InDelta(t, 0.3, gemini.FiveHour.Utilization, 1e-9)
	claude := got.AntigravityPools[1]
	require.NotNil(t, claude.FiveHour.WindowStats)
	require.Nil(t, claude.Weekly)

	require.Len(t, repo.calls, 3)
	// 窗口起点由重置时间反推
	require.True(t, repo.calls[0].start.Equal(fiveHourReset.Add(-5*time.Hour)))
	require.Equal(t, []string{"gemini"}, repo.calls[0].prefixes)
	require.True(t, repo.calls[1].start.Equal(weeklyReset.Add(-7*24*time.Hour)))
	require.Equal(t, []string{"claude", "gpt"}, repo.calls[2].prefixes)

	// 缓存里的原对象不被原地修改
	require.Nil(t, original.AntigravityPools[0].FiveHour.WindowStats)
	require.Nil(t, original.AntigravityPools[0].Weekly.WindowStats)

	// 1 分钟内再次查询命中缓存，不再打库
	svc.withAntigravityPoolWindowStats(context.Background(), &Account{ID: 7}, original)
	require.Len(t, repo.calls, 3)
}

func TestWithAntigravityPoolWindowStatsIdleWindowUsesRollingStart(t *testing.T) {
	repo := &agPoolStatsRepoStub{}
	svc := &AccountUsageService{usageLogRepo: repo, cache: NewUsageCache()}

	fetchedReset := time.Now().Add(5*time.Hour - 2*time.Minute)
	usage := &UsageInfo{AntigravityPools: []AntigravityPoolUsage{{
		Pool:     antigravityPoolGemini,
		FiveHour: &UsageProgress{ResetsAt: &fetchedReset, WindowIdle: true},
	}}}

	before := time.Now()
	got := svc.withAntigravityPoolWindowStats(context.Background(), &Account{ID: 9}, usage)
	require.NotNil(t, got.AntigravityPools[0].FiveHour.WindowStats)
	require.True(t, got.AntigravityPools[0].FiveHour.WindowIdle)
	require.Len(t, repo.calls, 1)
	// 起点是 now-5h，而不是 resetsAt-5h（≈ 两分钟前）
	require.WithinDuration(t, before.Add(-5*time.Hour), repo.calls[0].start, time.Second)
}

func TestWithAntigravityPoolWindowStatsWithoutReader(t *testing.T) {
	svc := &AccountUsageService{usageLogRepo: &usageBatchLogRepoStub{}, cache: NewUsageCache()}
	usage := &UsageInfo{AntigravityPools: []AntigravityPoolUsage{{Pool: antigravityPoolGemini, FiveHour: &UsageProgress{}}}}
	require.Same(t, usage, svc.withAntigravityPoolWindowStats(context.Background(), &Account{ID: 1}, usage))
}

func TestAntigravityWindowStart(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	length := 5 * time.Hour

	reset := now.Add(2 * time.Hour)
	require.Equal(t, now.Add(-3*time.Hour), antigravityWindowStart(&reset, false, length, now))

	// 没有 / 已过期 / 超出一个窗口长度的重置时间：回落为滚动窗口
	require.Equal(t, now.Add(-length), antigravityWindowStart(nil, false, length, now))
	past := now.Add(-time.Minute)
	require.Equal(t, now.Add(-length), antigravityWindowStart(&past, false, length, now))
	far := now.Add(6 * time.Hour)
	require.Equal(t, now.Add(-length), antigravityWindowStart(&far, false, length, now))

	// idle 窗口的 resetTime 是「取数时刻 + length」，反推会让起点贴着现在；按滚动窗口统计
	idleReset := now.Add(length - 2*time.Minute)
	require.Equal(t, now.Add(-length), antigravityWindowStart(&idleReset, true, length, now))
}
