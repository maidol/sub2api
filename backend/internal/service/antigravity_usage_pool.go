package service

import (
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

const (
	antigravityPoolGemini    = "gemini"
	antigravityPoolClaudeGPT = "claude_gpt"

	antigravityWindowFiveHour = "5h"
	antigravityWindowWeekly   = "weekly"

	antigravityPoolSourceSummary  = "quota_summary"
	antigravityPoolSourceInferred = "per_model_inferred"
)

// AntigravityPoolUsage 是一个家族共享池的窗口快照，对齐 Antigravity 客户端设置页的
// 「Gemini Models」/「Claude and GPT models」两组。
//
// Weekly 为 nil 表示上游本次没有下发周窗口——前端整行不渲染，不要填 0：
// 「本周用了 0」和「上游没给这个数」在页面上必须长得不一样。
type AntigravityPoolUsage struct {
	Pool     string         `json:"pool"`
	FiveHour *UsageProgress `json:"five_hour,omitempty"`
	Weekly   *UsageProgress `json:"weekly,omitempty"`
	Models   []string       `json:"models,omitempty"` // 归入该池的模型键，供排查
}

// antigravityPoolFor 把模型键归到家族池；未知家族返回 ""，不归池
// （它仍然出现在逐模型清单里，不会因为归组而消失）。
func antigravityPoolFor(modelKey string) string {
	k := strings.ToLower(modelKey)
	switch {
	case strings.HasPrefix(k, "gemini"):
		return antigravityPoolGemini
	case strings.HasPrefix(k, "claude"), strings.HasPrefix(k, "gpt"):
		return antigravityPoolClaudeGPT
	}
	return ""
}

// buildAntigravityPools 只在同池内每个模型的 (utilization, resetTime) 完全一致时
// 才把它们合并成一行；有一个不一致，整池就不出现在返回里，前端回落到逐模型清单。
//
// 这条一致性检查是「per-model 配额 == 家族池」这个假设的证伪点，不是防御性代码。
// 不要放宽成「取最大值」或「取第一个」——那样假设一旦被推翻，页面上不会有任何现象，
// 照样画出一条看起来很正常但是错的池。
func buildAntigravityPools(quota map[string]*AntigravityModelQuota) []AntigravityPoolUsage {
	if len(quota) == 0 {
		return nil
	}

	type agg struct {
		util      int
		resetTime string
		models    []string
		mixed     bool
	}
	byPool := make(map[string]*agg, 2)

	for key, q := range quota {
		if q == nil {
			continue
		}
		pool := antigravityPoolFor(key)
		if pool == "" {
			continue
		}
		a, ok := byPool[pool]
		if !ok {
			byPool[pool] = &agg{util: q.Utilization, resetTime: q.ResetTime, models: []string{key}}
			continue
		}
		if a.util != q.Utilization || a.resetTime != q.ResetTime {
			a.mixed = true
		}
		a.models = append(a.models, key)
	}

	// 顺序由这个固定数组给，不要靠 map 遍历——Go 的 map 顺序是随机的，
	// 靠它排序会让同一份数据每次刷新都换行序，而且测试会随机红。
	out := make([]AntigravityPoolUsage, 0, 2)
	for _, pool := range []string{antigravityPoolGemini, antigravityPoolClaudeGPT} {
		a := byPool[pool]
		if a == nil || a.mixed {
			continue
		}
		sort.Strings(a.models)
		out = append(out, AntigravityPoolUsage{
			Pool:     pool,
			FiveHour: antigravityProgress(float64(a.util), a.resetTime),
			Models:   a.models,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// antigravityProgress 抽自 buildUsageInfo:182-190 的 resetTime 解析，两处共用。
func antigravityProgress(utilization float64, resetTime string) *UsageProgress {
	p := &UsageProgress{Utilization: utilization}
	if resetTime == "" {
		return p
	}
	if t, err := time.Parse(time.RFC3339, resetTime); err == nil {
		p.ResetsAt = &t
		p.RemainingSeconds = int(time.Until(t).Seconds())
	}
	return p
}

// buildAntigravityPoolsFromSummary 把 retrieveUserQuotaSummary 的 groups 直接转成两池两窗口。
//
// 这是权威来源：分组和分窗口都是上游自己给的，不需要像 buildAntigravityPools 那样
// 从 per-model 数据反推。返回 nil 表示这份响应里一个能用的桶都没有——
// 调用方此时必须保留反推出来的池，而不是把页面清空。
func buildAntigravityPoolsFromSummary(summary *antigravity.RetrieveUserQuotaSummaryResponse) []AntigravityPoolUsage {
	if summary == nil || len(summary.Groups) == 0 {
		return nil
	}

	byPool := make(map[string]*AntigravityPoolUsage, 2)
	for gi := range summary.Groups {
		group := &summary.Groups[gi]
		for bi := range group.Buckets {
			bucket := &group.Buckets[bi]

			// 没有这个字段 ≠ 剩 0。跳过，让那一行不渲染。
			fraction, ok := bucket.GetRemainingFraction()
			if !ok {
				continue
			}
			pool := antigravitySummaryPool(group.DisplayName, bucket.BucketID)
			if pool == "" {
				continue
			}
			window := antigravitySummaryWindow(bucket.Window, bucket.BucketID)
			if window == "" {
				continue
			}

			entry := byPool[pool]
			if entry == nil {
				entry = &AntigravityPoolUsage{Pool: pool}
				byPool[pool] = entry
			}
			// 这里不经过 int 截断：summary 给的是 float64，保留它。
			progress := antigravityProgress((1.0-fraction)*100, bucket.ResetTime)
			switch window {
			case antigravityWindowFiveHour:
				entry.FiveHour = progress
			case antigravityWindowWeekly:
				entry.Weekly = progress
			}
		}
	}

	// 顺序仍由固定数组给，理由同 buildAntigravityPools。
	out := make([]AntigravityPoolUsage, 0, 2)
	for _, pool := range []string{antigravityPoolGemini, antigravityPoolClaudeGPT} {
		entry := byPool[pool]
		if entry == nil || (entry.FiveHour == nil && entry.Weekly == nil) {
			continue
		}
		out = append(out, *entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// antigravitySummaryPool 认组：先看 bucketId 前缀，再看 group 的 displayName。
// 两个都不认识就返回 ""——宁可少画一个池，也不要把陌生的桶塞进已知的池里。
func antigravitySummaryPool(groupDisplayName, bucketID string) string {
	id := strings.ToLower(strings.TrimSpace(bucketID))
	switch {
	case strings.HasPrefix(id, "gemini"):
		return antigravityPoolGemini
	case strings.HasPrefix(id, "3p"), strings.HasPrefix(id, "claude"), strings.HasPrefix(id, "gpt"):
		return antigravityPoolClaudeGPT
	}

	name := strings.ToLower(strings.TrimSpace(groupDisplayName))
	switch {
	case strings.Contains(name, "gemini"):
		return antigravityPoolGemini
	case strings.Contains(name, "claude"), strings.Contains(name, "gpt"):
		return antigravityPoolClaudeGPT
	}
	return ""
}

// antigravitySummaryWindow 认窗口：window 字段优先，缺了就看 bucketId 后缀。
func antigravitySummaryWindow(window, bucketID string) string {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "5h", "five_hour", "five-hour", "fivehour":
		return antigravityWindowFiveHour
	case "weekly", "week", "7d", "seven_day":
		return antigravityWindowWeekly
	}

	id := strings.ToLower(strings.TrimSpace(bucketID))
	switch {
	case strings.HasSuffix(id, "-weekly"), strings.HasSuffix(id, "_weekly"):
		return antigravityWindowWeekly
	case strings.HasSuffix(id, "-5h"), strings.HasSuffix(id, "_5h"):
		return antigravityWindowFiveHour
	}
	return ""
}

// logAntigravityPoolDivergence 在两个来源都拿到时比一次 5h 窗口。
//
// 「per-model 配额就是家族 5h 池」这个假设，在有权威来源之前没有裁判；现在有了。
// 差超过 1 个百分点就是假设被推翻的现场——只记日志不改行为，因为权威那份本来就该赢。
func logAntigravityPoolDivergence(inferred, authoritative []AntigravityPoolUsage) {
	byPool := make(map[string]*UsageProgress, len(inferred))
	for i := range inferred {
		byPool[inferred[i].Pool] = inferred[i].FiveHour
	}
	for i := range authoritative {
		a := authoritative[i].FiveHour
		b := byPool[authoritative[i].Pool]
		if a == nil || b == nil {
			continue
		}
		if diff := a.Utilization - b.Utilization; diff > 1 || diff < -1 {
			slog.Warn("antigravity pool 5h utilization diverges between sources",
				"pool", authoritative[i].Pool,
				"quota_summary", a.Utilization,
				"per_model_inferred", b.Utilization)
		}
	}
}
