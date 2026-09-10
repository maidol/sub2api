//go:build unit

package service

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

// 1. 正向：Step 0 的真实 fixture
func TestBuildAntigravityPoolsFromSummary_RealFixture(t *testing.T) {
	fixtureData, err := os.ReadFile("testdata/antigravity_quota_summary.json")
	require.NoError(t, err)

	var summary antigravity.RetrieveUserQuotaSummaryResponse
	err = json.Unmarshal(fixtureData, &summary)
	require.NoError(t, err)

	pools := buildAntigravityPoolsFromSummary(&summary)
	require.Len(t, pools, 2)

	// 固定顺序：Gemini 在前，Claude/GPT 在后
	require.Equal(t, antigravityPoolGemini, pools[0].Pool)
	require.NotNil(t, pools[0].FiveHour)
	require.NotNil(t, pools[0].Weekly)
	require.InDelta(t, (1.0-0.7382173)*100, pools[0].FiveHour.Utilization, 0.0001)
	require.InDelta(t, (1.0-0.662049)*100, pools[0].Weekly.Utilization, 0.0001)

	require.Equal(t, antigravityPoolClaudeGPT, pools[1].Pool)
	require.NotNil(t, pools[1].FiveHour)
	require.NotNil(t, pools[1].Weekly)
	require.InDelta(t, (1.0-1.0)*100, pools[1].FiveHour.Utilization, 0.0001)
	require.InDelta(t, (1.0-0.41361254)*100, pools[1].Weekly.Utilization, 0.0001)
}

// 2. 反向：桶里没有 remainingFraction 键 → 那个窗口是 nil，不是 0
func TestBuildAntigravityPoolsFromSummary_RefusesMissingRemainingFraction(t *testing.T) {
	summary := &antigravity.RetrieveUserQuotaSummaryResponse{
		Groups: []antigravity.QuotaSummaryGroup{
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravity.QuotaSummaryBucket{
					{
						BucketID:  "gemini-5h",
						Window:    "5h",
						ResetTime: "2026-09-09T20:38:41Z",
						// RemainingFraction is nil
					},
					{
						BucketID:          "gemini-weekly",
						Window:            "weekly",
						ResetTime:         "2026-09-11T17:43:53Z",
						RemainingFraction: floatVal(0.5),
					},
				},
			},
		},
	}

	pools := buildAntigravityPoolsFromSummary(summary)
	require.Len(t, pools, 1)
	require.Nil(t, pools[0].FiveHour, "缺失 remainingFraction 必须是 nil，绝不能当成 0 (已用满100%)")
	require.NotNil(t, pools[0].Weekly)
	require.InDelta(t, 50.0, pools[0].Weekly.Utilization, 0.0001)
}

// 3. 反向：只有 gemini-5h，没有 gemini-weekly → Weekly 为 nil，FiveHour 正常
func TestBuildAntigravityPoolsFromSummary_OnlyFiveHour(t *testing.T) {
	summary := &antigravity.RetrieveUserQuotaSummaryResponse{
		Groups: []antigravity.QuotaSummaryGroup{
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravity.QuotaSummaryBucket{
					{
						BucketID:          "gemini-5h",
						Window:            "5h",
						ResetTime:         "2026-09-09T20:38:41Z",
						RemainingFraction: floatVal(0.8),
					},
				},
			},
		},
	}

	pools := buildAntigravityPoolsFromSummary(summary)
	require.Len(t, pools, 1)
	require.NotNil(t, pools[0].FiveHour)
	require.Nil(t, pools[0].Weekly, "未下发 weekly 时必须是 nil")
	require.InDelta(t, 20.0, pools[0].FiveHour.Utilization, 0.0001)
}

// 4. 嵌套形状 {"remaining":{"remainingFraction":0.72}} → 解析出与扁平形状相同数值
func TestBuildAntigravityPoolsFromSummary_NestedRemainingFraction(t *testing.T) {
	summary := &antigravity.RetrieveUserQuotaSummaryResponse{
		Groups: []antigravity.QuotaSummaryGroup{
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravity.QuotaSummaryBucket{
					{
						BucketID:  "gemini-5h",
						Window:    "5h",
						ResetTime: "2026-09-09T20:38:41Z",
						Remaining: &struct {
							RemainingFraction *float64 `json:"remainingFraction,omitempty"`
						}{
							RemainingFraction: floatVal(0.72),
						},
					},
				},
			},
		},
	}

	pools := buildAntigravityPoolsFromSummary(summary)
	require.Len(t, pools, 1)
	require.NotNil(t, pools[0].FiveHour)
	require.InDelta(t, 28.0, pools[0].FiveHour.Utilization, 0.0001)
}

// 5. 反向：陌生组（displayName:"Foo"、bucketId:"foo-5h"）→ 不进任何池且不影响其它池
func TestBuildAntigravityPoolsFromSummary_IgnoresUnknownGroup(t *testing.T) {
	summary := &antigravity.RetrieveUserQuotaSummaryResponse{
		Groups: []antigravity.QuotaSummaryGroup{
			{
				DisplayName: "Unknown Models Foo",
				Buckets: []antigravity.QuotaSummaryBucket{
					{
						BucketID:          "foo-5h",
						Window:            "5h",
						RemainingFraction: floatVal(0.9),
					},
				},
			},
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravity.QuotaSummaryBucket{
					{
						BucketID:          "gemini-5h",
						Window:            "5h",
						ResetTime:         "2026-09-09T20:38:41Z",
						RemainingFraction: floatVal(0.8),
					},
				},
			},
		},
	}

	pools := buildAntigravityPoolsFromSummary(summary)
	require.Len(t, pools, 1)
	require.Equal(t, antigravityPoolGemini, pools[0].Pool)
}

// 6. 反向：summary 为 nil / groups: [] → 返回 nil
func TestBuildAntigravityPoolsFromSummary_EmptyOrNil(t *testing.T) {
	require.Nil(t, buildAntigravityPoolsFromSummary(nil))
	require.Nil(t, buildAntigravityPoolsFromSummary(&antigravity.RetrieveUserQuotaSummaryResponse{}))
	require.Nil(t, buildAntigravityPoolsFromSummary(&antigravity.RetrieveUserQuotaSummaryResponse{
		Groups: []antigravity.QuotaSummaryGroup{},
	}))
}

// 7. window 字段缺失、bucketId 是 gemini-weekly → 识别为 weekly
func TestBuildAntigravityPoolsFromSummary_WindowFallbackToBucketID(t *testing.T) {
	summary := &antigravity.RetrieveUserQuotaSummaryResponse{
		Groups: []antigravity.QuotaSummaryGroup{
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravity.QuotaSummaryBucket{
					{
						BucketID:          "gemini-weekly",
						Window:            "", // 缺失
						RemainingFraction: floatVal(0.6),
					},
				},
			},
		},
	}

	pools := buildAntigravityPoolsFromSummary(summary)
	require.Len(t, pools, 1)
	require.Nil(t, pools[0].FiveHour)
	require.NotNil(t, pools[0].Weekly)
	require.InDelta(t, 40.0, pools[0].Weekly.Utilization, 0.0001)
}

// 8. 桶的 resetTime 为空 → ResetsAt 为 nil，RemainingSeconds 为 0，且不 panic
func TestBuildAntigravityPoolsFromSummary_EmptyResetTime(t *testing.T) {
	summary := &antigravity.RetrieveUserQuotaSummaryResponse{
		Groups: []antigravity.QuotaSummaryGroup{
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravity.QuotaSummaryBucket{
					{
						BucketID:          "gemini-5h",
						Window:            "5h",
						ResetTime:         "", // 空
						RemainingFraction: floatVal(0.5),
					},
				},
			},
		},
	}

	pools := buildAntigravityPoolsFromSummary(summary)
	require.Len(t, pools, 1)
	require.NotNil(t, pools[0].FiveHour)
	require.Nil(t, pools[0].FiveHour.ResetsAt)
	require.Equal(t, 0, pools[0].FiveHour.RemainingSeconds)
}

// 9. 反向：buildAntigravityPoolsFromSummary 返回空时，usageInfo.AntigravityPools 保持 per-model 反推的那份
func TestAntigravityPoolFallback_SummaryFailsRetainsInferred(t *testing.T) {
	inferredPools := []AntigravityPoolUsage{
		{
			Pool:     antigravityPoolGemini,
			FiveHour: &UsageProgress{Utilization: 10},
			Models:   []string{"gemini-2.5-pro"},
		},
	}

	usageInfo := &UsageInfo{
		AntigravityPools:      inferredPools,
		AntigravityPoolSource: antigravityPoolSourceInferred,
	}

	// 假设 summary 返回空（不可用）
	var summary *antigravity.RetrieveUserQuotaSummaryResponse = nil
	if pools := buildAntigravityPoolsFromSummary(summary); len(pools) > 0 {
		usageInfo.AntigravityPools = pools
		usageInfo.AntigravityPoolSource = antigravityPoolSourceSummary
	}

	require.Len(t, usageInfo.AntigravityPools, 1)
	require.Equal(t, antigravityPoolSourceInferred, usageInfo.AntigravityPoolSource)
	require.Equal(t, float64(10), usageInfo.AntigravityPools[0].FiveHour.Utilization)
}

func floatVal(v float64) *float64 {
	return &v
}

func TestLogAntigravityPoolDivergence(t *testing.T) {
	inferred := []AntigravityPoolUsage{
		{
			Pool:     antigravityPoolGemini,
			FiveHour: &UsageProgress{Utilization: 10},
		},
	}
	authoritative := []AntigravityPoolUsage{
		{
			Pool:     antigravityPoolGemini,
			FiveHour: &UsageProgress{Utilization: 15}, // 差 5%
		},
	}

	// 不应该 panic
	require.NotPanics(t, func() {
		logAntigravityPoolDivergence(inferred, authoritative)
		logAntigravityPoolDivergence(nil, authoritative)
		logAntigravityPoolDivergence(inferred, nil)
	})
}
