package service

import (
	"sort"
	"strings"
	"time"
)

const (
	antigravityPoolGemini    = "gemini"
	antigravityPoolClaudeGPT = "claude_gpt"
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
