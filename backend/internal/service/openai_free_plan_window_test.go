//go:build unit

package service

import (
	"testing"
	"time"
)

// 现场：ChatGPT free 套餐的 /wham/usage 只返回一个窗口，
// limit_window_seconds = 2592000（30 天），secondary_window 为显式 null；
// 真实请求的响应头口径相同：x-codex-primary-window-minutes=43200、
// x-codex-secondary-window-minutes=0。
//
// 这个 30 天窗口被 Normalize() 按 ">360 分钟" 归一化进了 7d 槽位，
// 下面两组测试钉住由此产生的两个缺陷。

// codexWindowStatsStart 必须用窗口的真实长度反推起点。
// 修复前：用名义的 7 天去减一个 30 天窗口的 resetsAt，起点落在未来 21 天，
// 统计区间 [未来, now] 必然为空 → 「7天」栏的请求数/token 数恒为 0。
func TestCodexWindowStatsStart_UsesRealWindowMinutesNotSlotNominal(t *testing.T) {
	now := time.Date(2026, 9, 11, 7, 57, 57, 0, time.UTC)
	resetsAt := time.Date(2026, 10, 9, 10, 22, 52, 0, time.UTC)

	progress := &UsageProgress{
		Utilization:   49,
		ResetsAt:      &resetsAt,
		WindowMinutes: 43200, // 30 天：上游声明的真实长度
	}

	start := codexWindowStatsStart(progress, 7*24*time.Hour, now)

	if start.After(now) {
		t.Fatalf("窗口起点落在未来 (%s > %s)：统计区间为空，"+
			"「7天」栏的本地统计会恒为 0", start, now)
	}
	want := resetsAt.Add(-43200 * time.Minute) // 2026-09-09T10:22:52Z
	if !start.Equal(want) {
		t.Fatalf("codexWindowStatsStart = %s, want %s (resets_at 减去真实窗口长度)", start, want)
	}
}

// 拿不到真实长度时（WindowMinutes 为 0），仍然不允许返回未来的起点。
// 这道下限是兜底：宁可统计区间偏短，也不能给出一个空区间。
func TestCodexWindowStatsStart_NeverReturnsFutureStartWhenWindowUnknown(t *testing.T) {
	now := time.Date(2026, 9, 11, 7, 57, 57, 0, time.UTC)
	resetsAt := time.Date(2026, 10, 9, 10, 22, 52, 0, time.UTC)

	progress := &UsageProgress{
		Utilization: 49,
		ResetsAt:    &resetsAt,
		// WindowMinutes 缺失：老数据、或上游没给窗口长度
	}

	start := codexWindowStatsStart(progress, 7*24*time.Hour, now)

	if start.After(now) {
		t.Fatalf("窗口起点落在未来 (%s > %s)：统计区间为空", start, now)
	}
	want := now.Add(-7 * 24 * time.Hour)
	if !start.Equal(want) {
		t.Fatalf("codexWindowStatsStart = %s, want %s (回退到 now 减名义长度)", start, want)
	}
}

// 名义长度与真实长度一致时（订阅号的 7d 窗口），行为不得改变。
// 这条是反向对照：防止「一律回退到 now 减名义长度」这种把测试骗绿的错修法。
func TestCodexWindowStatsStart_RealSevenDayWindowUnchanged(t *testing.T) {
	now := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	resetsAt := now.Add(48 * time.Hour)

	progress := &UsageProgress{
		Utilization:   30,
		ResetsAt:      &resetsAt,
		WindowMinutes: 10080, // 7 天
	}

	start := codexWindowStatsStart(progress, 7*24*time.Hour, now)

	want := resetsAt.Add(-7 * 24 * time.Hour)
	if !start.Equal(want) {
		t.Fatalf("codexWindowStatsStart = %s, want %s；"+
			"真实窗口就是 7 天时必须仍按 resets_at 反推，不能退化成 now-7d", start, want)
	}
}

// window_minutes 为 0 表示「这个窗口不存在」，不是「一个长度为 0 的窗口」。
// 修复前：ptr(0) 被当成有效窗口，零值一路写进 codex_5h_*。
func TestNormalize_SecondaryWindowMinutesZeroMeansNoSuchWindow(t *testing.T) {
	primaryUsed := 49.0
	primaryReset := 2428562
	primaryWindow := 43200
	secondaryUsed := 0.0
	secondaryReset := 0
	secondaryWindow := 0

	snapshot := &OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:         &primaryUsed,
		PrimaryResetAfterSeconds:   &primaryReset,
		PrimaryWindowMinutes:       &primaryWindow,
		SecondaryUsedPercent:       &secondaryUsed,
		SecondaryResetAfterSeconds: &secondaryReset,
		SecondaryWindowMinutes:     &secondaryWindow,
		UpdatedAt:                  "2026-09-11T07:55:32Z",
	}

	normalized := snapshot.Normalize()
	if normalized == nil {
		t.Fatal("expected non-nil normalized limits")
	}

	// 30 天窗口仍然落在 7d 槽（本次不改归一化的分槽规则）
	if normalized.Used7dPercent == nil || *normalized.Used7dPercent != 49.0 {
		t.Fatalf("Used7dPercent = %v, want 49", normalized.Used7dPercent)
	}

	// 5h 槽必须整组为空，而不是 0
	if normalized.Used5hPercent != nil {
		t.Fatalf("Used5hPercent = %v, want nil；window_minutes=0 表示窗口不存在，"+
			"写成 0%% 会让前端渲染出一条永远 0%% 的假进度条", *normalized.Used5hPercent)
	}
	if normalized.Window5hMinutes != nil {
		t.Fatalf("Window5hMinutes = %v, want nil", *normalized.Window5hMinutes)
	}
	if normalized.Reset5hSeconds != nil {
		t.Fatalf("Reset5hSeconds = %v, want nil", *normalized.Reset5hSeconds)
	}
}

// 端到端：free 套餐的响应头快照不得写出任何 codex_5h_* 键。
// 修复前 codex_5h_reset_at 会被写成「此刻」，每来一次请求就往前跳一次。
func TestBuildCodexUsageExtraUpdates_FreePlanWritesNoFiveHourKeys(t *testing.T) {
	primaryUsed := 49.0
	primaryReset := 2428562
	primaryWindow := 43200
	secondaryUsed := 0.0
	secondaryReset := 0
	secondaryWindow := 0

	snapshot := &OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:         &primaryUsed,
		PrimaryResetAfterSeconds:   &primaryReset,
		PrimaryWindowMinutes:       &primaryWindow,
		SecondaryUsedPercent:       &secondaryUsed,
		SecondaryResetAfterSeconds: &secondaryReset,
		SecondaryWindowMinutes:     &secondaryWindow,
		UpdatedAt:                  "2026-09-11T07:55:32Z",
	}

	updates := buildCodexUsageExtraUpdates(snapshot, time.Date(2026, 9, 11, 7, 55, 32, 0, time.UTC))
	if updates == nil {
		t.Fatal("expected non-nil updates")
	}

	for _, key := range []string{
		"codex_5h_used_percent",
		"codex_5h_reset_after_seconds",
		"codex_5h_window_minutes",
		"codex_5h_reset_at",
	} {
		v, ok := updates[key]
		if !ok || v != nil {
			t.Fatalf("updates[%q] = %v (present=%v), want nil deletion marker：free 套餐没有 5h 窗口", key, v, ok)
		}
	}

	// 30 天窗口本身必须照常落库（排查用的 primary 原始字段也保留）
	if got := updates["codex_7d_used_percent"]; got != 49.0 {
		t.Fatalf("codex_7d_used_percent = %v, want 49", got)
	}
	if got := updates["codex_7d_window_minutes"]; got != 43200 {
		t.Fatalf("codex_7d_window_minutes = %v, want 43200", got)
	}
	if got := updates["codex_primary_used_percent"]; got != 49.0 {
		t.Fatalf("codex_primary_used_percent = %v, want 49（原始字段保留用于排查）", got)
	}
}

// UsageProgress.WindowMinutes 必须能从 Extra 读出来，
// 否则 codexWindowStatsStart 永远拿不到真实长度，等于没修。
func TestBuildCodexUsageProgressFromExtra_CarriesWindowMinutes(t *testing.T) {
	now := time.Date(2026, 9, 11, 7, 57, 57, 0, time.UTC)
	extra := map[string]any{
		"codex_7d_used_percent":        49,
		"codex_7d_window_minutes":      43200,
		"codex_7d_reset_at":            "2026-10-09T10:22:52Z",
		"codex_usage_updated_at":       "2026-09-11T07:55:32Z",
		"codex_primary_used_percent":   49,
		"codex_primary_window_minutes": 43200,
	}

	progress := buildCodexUsageProgressFromExtra(extra, "7d", now)
	if progress == nil {
		t.Fatal("expected non-nil progress")
	}
	if progress.WindowMinutes != 43200 {
		t.Fatalf("WindowMinutes = %d, want 43200；"+
			"拿不到真实长度时 codexWindowStatsStart 会退回名义 7 天", progress.WindowMinutes)
	}
}
