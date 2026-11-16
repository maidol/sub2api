//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestQuotaMergeMixedFamiliesFallBackToModels(t *testing.T) {
	const reset = "2099-11-15T18:00:00Z"
	const otherReset = "2099-11-15T19:00:00Z"
	for _, tc := range []struct {
		name      string
		model     string
		remaining float64
		reset     string
		used      int
	}{
		{"gemini_utilization", "gemini-3.8-pro", 0.25, reset, 75},
		{"gemini_reset", "gemini-3.8-pro", 0.75, otherReset, 25},
		{"argon_utilization", "gemini-4-argon-high", 0.25, reset, 75},
		{"argon_reset", "gemini-4-argon-high", 0.5, otherReset, 50},
		{"claude_utilization", "gpt-review", 0.25, reset, 75},
		{"claude_reset", "gpt-review", 0.75, otherReset, 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := map[string]antigravity.ModelInfo{}
			for key, fraction := range map[string]float64{
				"gemini-3.8-flash":    0.75,
				"gemini-3.8-pro":      0.75,
				"gemini-4-argon":      0.5,
				"gemini-4-argon-high": 0.5,
				"claude-review":       0.75,
				"gpt-review":          0.75,
			} {
				models[key] = antigravity.ModelInfo{QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: fraction, ResetTime: reset,
				}}
			}
			models[tc.model].QuotaInfo.RemainingFraction = tc.remaining
			models[tc.model].QuotaInfo.ResetTime = tc.reset
			fetcher := &AntigravityQuotaFetcher{}
			info := fetcher.buildUsageInfo(&antigravity.FetchAvailableModelsResponse{Models: models}, "g1-pro-tier", "PRO", nil)
			if tc.name == "gemini_utilization" {
				body, err := json.Marshal(info)
				require.NoError(t, err)
				t.Logf("POOL_FALLBACK_USAGE=%s", body)
			}
			require.Nil(t, info.AntigravityPools, "one mixed family must force the whole inferred result to per-model mode")
			require.Empty(t, info.AntigravityPoolSource)
			require.Len(t, info.AntigravityQuota, 6)
			require.Len(t, info.AntigravityQuotaDetails, 6)
			wantUsed := map[string]int{
				"gemini-3.8-flash": 25, "gemini-3.8-pro": 25,
				"gemini-4-argon": 50, "gemini-4-argon-high": 50,
				"claude-review": 25, "gpt-review": 25,
			}
			wantUsed[tc.model] = tc.used
			for key, used := range wantUsed {
				require.NotNil(t, info.AntigravityQuota[key])
				require.Equal(t, used, info.AntigravityQuota[key].Utilization)
				wantReset := reset
				if key == tc.model {
					wantReset = tc.reset
				}
				require.Equal(t, wantReset, info.AntigravityQuota[key].ResetTime)
			}
		})
	}
}

func TestQuotaMergeArgonDoesNotOverwriteGemini(t *testing.T) {
	var summary antigravity.QuotaSummaryResponse
	require.NoError(t, json.Unmarshal([]byte(`{"groups":[
		{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.75}]},
		{"displayName":"Gemini Argon Models","buckets":[{"bucketId":"gemini-argon-weekly","window":"weekly","remainingFraction":0.4}]}
	]}`), &summary))
	pools := buildAntigravityPoolsFromSummary(&summary)
	require.Len(t, pools, 2)
	require.Equal(t, "gemini", pools[0].Pool)
	require.InDelta(t, 25.0, pools[0].Weekly.Utilization, 0.001)
	require.Equal(t, "gemini_argon", pools[1].Pool)
	require.InDelta(t, 60.0, pools[1].Weekly.Utilization, 0.001)
}

func TestQuotaMergeMissingAndNestedFractions(t *testing.T) {
	var summary antigravity.QuotaSummaryResponse
	require.NoError(t, json.Unmarshal([]byte(`{"groups":[
		{"displayName":"Missing","buckets":[{"bucketId":"gemini-5h","window":"5h"}]},
		{"displayName":"Gemini Models","buckets":[
			{"bucketId":"gemini-5h","window":"5h","remaining":{"remainingFraction":0.4}},
			{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0}
		]}
	]}`), &summary))
	groups := buildAntigravityQuotaGroups(&summary)
	require.Len(t, groups, 1, "missing fraction must not invent a full quota window")
	require.Len(t, groups[0].Windows, 2)
	require.InDelta(t, 60.0, groups[0].Windows[0].Utilization, 0.001)
	require.Equal(t, 100.0, groups[0].Windows[1].Utilization, "explicit zero is exhausted")
}

func TestQuotaMergeInferredUltraAndUnknownShareGemini(t *testing.T) {
	for _, tier := range []string{"ULTRA", "UNKNOWN", ""} {
		pools := buildAntigravityPools(map[string]*AntigravityModelQuota{
			"gemini-3.8-flash": {Utilization: 25},
			"gemini-4-argon":   {Utilization: 25},
		}, tier)
		require.Len(t, pools, 1)
		require.Equal(t, "gemini", pools[0].Pool)
		require.Len(t, pools[0].Models, 2)
	}
}

func TestQuotaMergeInferredArgonIsSeparate(t *testing.T) {
	pools := buildAntigravityPools(map[string]*AntigravityModelQuota{
		"gemini-3.8-flash":    {Utilization: 25},
		"gemini-4-argon":      {Utilization: 60},
		"gemini-4-argon-high": {Utilization: 60},
	}, "PRO")
	require.Len(t, pools, 2)
	require.Equal(t, "gemini", pools[0].Pool)
	require.Equal(t, "gemini_argon", pools[1].Pool)
	require.Equal(t, []string{"gemini-4-argon", "gemini-4-argon-high"}, pools[1].Models)
}
