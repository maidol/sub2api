//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const poolResetA = "2026-09-09T12:00:00Z"
const poolResetB = "2026-09-09T13:00:00Z"

func geminiQuota() map[string]*AntigravityModelQuota {
	return map[string]*AntigravityModelQuota{
		"gemini-3.8-flash-tiered": {Utilization: 4, ResetTime: poolResetA},
		"gemini-3.7-flash-tiered": {Utilization: 4, ResetTime: poolResetA},
		"gemini-2.5-pro":          {Utilization: 4, ResetTime: poolResetA},
	}
}

// 正向：同池齐平 → 合并成一行
func TestBuildAntigravityPools_MergesUniformFamily(t *testing.T) {
	pools := buildAntigravityPools(geminiQuota())

	require.Len(t, pools, 1)
	require.Equal(t, antigravityPoolGemini, pools[0].Pool)
	require.NotNil(t, pools[0].FiveHour)
	require.Equal(t, float64(4), pools[0].FiveHour.Utilization)
	require.Len(t, pools[0].Models, 3)
	require.Nil(t, pools[0].Weekly, "上游未下发周窗口时必须是 nil，不能是 0 值")
}

// 反向①：同池 utilization 不一致 → 整池不合并。
// 这是唯一能让归池逻辑变红的输入，也就是「per-model 配额 == 家族池」被推翻的现场。
func TestBuildAntigravityPools_RefusesMixedUtilization(t *testing.T) {
	quota := geminiQuota()
	quota["gemini-2.5-pro"].Utilization = 7

	require.Empty(t, buildAntigravityPools(quota))
}

// 反向②：resetTime 不一致 → 同样不合并
func TestBuildAntigravityPools_RefusesMixedResetTime(t *testing.T) {
	quota := geminiQuota()
	quota["gemini-2.5-pro"].ResetTime = poolResetB

	require.Empty(t, buildAntigravityPools(quota))
}

// 未知家族不进池，也不影响其它池
func TestBuildAntigravityPools_IgnoresUnknownFamily(t *testing.T) {
	quota := geminiQuota()
	quota["foo-bar-9"] = &AntigravityModelQuota{Utilization: 88, ResetTime: poolResetB}

	pools := buildAntigravityPools(quota)
	require.Len(t, pools, 1)
	require.NotContains(t, pools[0].Models, "foo-bar-9")
}

// 两池都在时，顺序固定 gemini 在前（不依赖 map 遍历顺序）
func TestBuildAntigravityPools_StableOrder(t *testing.T) {
	quota := geminiQuota()
	quota["claude-sonnet-4-20250514"] = &AntigravityModelQuota{Utilization: 0, ResetTime: poolResetB}
	quota["gpt-5.6"] = &AntigravityModelQuota{Utilization: 0, ResetTime: poolResetB}

	for i := 0; i < 20; i++ { // map 顺序随机，跑多次才能暴露顺序依赖
		pools := buildAntigravityPools(quota)
		require.Len(t, pools, 2)
		require.Equal(t, antigravityPoolGemini, pools[0].Pool)
		require.Equal(t, antigravityPoolClaudeGPT, pools[1].Pool)
	}
}

func TestBuildAntigravityPools_EmptyInput(t *testing.T) {
	require.Nil(t, buildAntigravityPools(nil))
	require.Nil(t, buildAntigravityPools(map[string]*AntigravityModelQuota{}))
}

func TestRecalcAntigravityRemainingSeconds(t *testing.T) {
	future1 := time.Now().Add(1 * time.Hour)
	future2 := time.Now().Add(2 * time.Hour)
	past := time.Now().Add(-10 * time.Minute)

	info := &UsageInfo{
		FiveHour: &UsageProgress{
			ResetsAt:         &future1,
			RemainingSeconds: 9999, // 过期值
		},
		AntigravityPools: []AntigravityPoolUsage{
			{
				Pool: antigravityPoolGemini,
				FiveHour: &UsageProgress{
					ResetsAt:         &future1,
					RemainingSeconds: 9999, // 过期值
				},
				Weekly: &UsageProgress{
					ResetsAt:         &future2,
					RemainingSeconds: 9999, // 过期值
				},
			},
			{
				Pool: antigravityPoolClaudeGPT,
				FiveHour: &UsageProgress{
					ResetsAt:         &past,
					RemainingSeconds: 9999, // 已过期
				},
				Weekly: nil,
			},
		},
	}

	recalcAntigravityRemainingSeconds(info)

	// info.FiveHour
	require.InDelta(t, 3600, info.FiveHour.RemainingSeconds, 5)

	// Gemini pool FiveHour and Weekly
	require.InDelta(t, 3600, info.AntigravityPools[0].FiveHour.RemainingSeconds, 5)
	require.InDelta(t, 7200, info.AntigravityPools[0].Weekly.RemainingSeconds, 5)

	// Claude/GPT pool FiveHour (past should be clamped to 0)
	require.Equal(t, 0, info.AntigravityPools[1].FiveHour.RemainingSeconds)
	require.Nil(t, info.AntigravityPools[1].Weekly)

	// nil safe check
	require.NotPanics(t, func() {
		recalcAntigravityRemainingSeconds(nil)
		recalcAntigravityRemainingSeconds(&UsageInfo{})
	})
}
