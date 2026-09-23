//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// Antigravity 暴露的 Gemini Flash 家族与各自的档位后缀。注册表、模型目录与计费
// 三处共用这一份清单，新增家族时只改这里。
var antigravityGeminiFlashFamilies = []string{
	"gemini-3.6-flash",
	"gemini-3.7-flash",
	"gemini-3.8-flash",
}

var antigravityGeminiFlashTiers = []string{"", "-high", "-low", "-medium", "-tiered"}

func antigravityGeminiFlashModelIDs() []string {
	ids := make([]string, 0, len(antigravityGeminiFlashFamilies)*len(antigravityGeminiFlashTiers))
	for _, family := range antigravityGeminiFlashFamilies {
		for _, tier := range antigravityGeminiFlashTiers {
			ids = append(ids, family+tier)
		}
	}
	return ids
}

// 注册表与对外模型目录必须收录全部档位：少一个，该 ID 的请求就会被当成未知模型。
func TestAntigravityGeminiFlash_RegisteredInModelMapping(t *testing.T) {
	for _, id := range antigravityGeminiFlashModelIDs() {
		if _, ok := domain.DefaultAntigravityModelMapping[id]; !ok {
			t.Errorf("%s 不在 DefaultAntigravityModelMapping 里", id)
		}
	}
}

func TestAntigravityGeminiFlash_ExposedInModelCatalog(t *testing.T) {
	exposed := make(map[string]bool)
	for _, m := range antigravity.DefaultModels() {
		exposed[m.ID] = true
	}
	for _, id := range antigravityGeminiFlashModelIDs() {
		if !exposed[id] {
			t.Errorf("%s 没有出现在 antigravity.DefaultModels() 里", id)
		}
	}
}

// 每个档位都必须算得出非零费用。价卡缺失时 CalculateCost 会按 0 记账，
// 带 token 的请求就此静默免费，且不会有任何报错暴露出来。
func TestAntigravityGeminiFlash_EveryTierIsBillable(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	tokens := UsageTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000}

	for _, id := range antigravityGeminiFlashModelIDs() {
		t.Run(id, func(t *testing.T) {
			cost, err := svc.CalculateCost(id, tokens, 1)
			require.NoErrorf(t, err, "%s 没有可用价卡", id)
			require.NotNil(t, cost)
			require.Greater(t, cost.TotalCost, 0.0)
		})
	}
}

// 3.7/3.8 Flash 的 priority/fast 档必须与标准档同价。
//
// 上游没有为这两个家族在价格目录里建条目，于是它们始终走 fallbackPrices；
// 那两张卡的 *PricePerTokenPriority 字段全部留空，usePriorityServiceTierPricing
// 因此返回 false，priority 请求落回标准价——这正是想要的结果，但它是"字段恰好
// 为零"的副产物，代码里没有任何一处声明过这个意图。一旦有人给目录补上带
// input_cost_per_token_priority 的条目（Google 公布 service tier 费率时很可能
// 发生），计费会直接翻倍且没有任何提示。这个用例就是那条声明。
func TestAntigravityGeminiFlash_PriorityTierBilledAtStandardPrice(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	tokens := UsageTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000}

	for _, family := range []string{"gemini-3.7-flash", "gemini-3.8-flash"} {
		for _, tier := range antigravityGeminiFlashTiers {
			model := family + tier
			t.Run(model, func(t *testing.T) {
				standard, err := svc.CalculateCostWithServiceTier(model, tokens, 1, "")
				require.NoError(t, err)
				for _, requested := range []string{"priority", "fast"} {
					got, err := svc.CalculateCostWithServiceTier(model, tokens, 1, requested)
					require.NoError(t, err)
					require.InDeltaf(t, standard.TotalCost, got.TotalCost, 1e-12,
						"%s 的 %s 档与标准档价格不一致", model, requested)
				}
			})
		}
	}
}

// 反向断言：3.6 Flash 在价格目录里是有 priority 费率的（1.8x），不能被上面那条
// 规则误伤。两条一起才说明"同价"是针对特定家族的判断，而不是 priority 通道整个失灵。
func TestAntigravityGeminiFlash_36PriorityKeepsCatalogRatio(t *testing.T) {
	pricingSvc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"gemini-3.6-flash": {
			InputCostPerToken:               1.5e-6,
			InputCostPerTokenPriority:       2.7e-6,
			OutputCostPerToken:              7.5e-6,
			OutputCostPerTokenPriority:      13.5e-6,
			CacheReadInputTokenCost:         0.15e-6,
			CacheReadInputTokenCostPriority: 0.27e-6,
		},
	}}
	svc := NewBillingService(&config.Config{}, pricingSvc)
	tokens := UsageTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000}

	standard, err := svc.CalculateCostWithServiceTier("gemini-3.6-flash", tokens, 1, "")
	require.NoError(t, err)
	priority, err := svc.CalculateCostWithServiceTier("gemini-3.6-flash", tokens, 1, "priority")
	require.NoError(t, err)
	require.InDelta(t, 1.8, priority.TotalCost/standard.TotalCost, 1e-12)
}

// Antigravity 上游回的是完整资源路径而非裸模型名。同价规则必须在归一化之后
// 仍然成立，否则 projects/.../models/gemini-3.8-flash 会绕开它按 priority 计价。
func TestAntigravityGeminiFlash_ResourcePathsKeepStandardPriorityPrice(t *testing.T) {
	pricingSvc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"gemini-3.7-flash": {
			InputCostPerToken:       0.75e-6,
			OutputCostPerToken:      3.75e-6,
			CacheReadInputTokenCost: 0.075e-6,
		},
		"gemini-3.8-flash": {
			InputCostPerToken:       0.75e-6,
			OutputCostPerToken:      3.75e-6,
			CacheReadInputTokenCost: 0.075e-6,
		},
	}}
	svc := NewBillingService(&config.Config{}, pricingSvc)
	tokens := UsageTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000}

	for _, model := range []string{
		"models/gemini-3.7-flash",
		"publishers/google/models/gemini-3.7-flash-high",
		"projects/p/locations/l/publishers/google/models/gemini-3.8-flash",
		"projects/p/locations/l/publishers/google/models/gemini-3.8-flash-tiered",
	} {
		t.Run(model, func(t *testing.T) {
			standard, err := svc.CalculateCostWithServiceTier(model, tokens, 1, "")
			require.NoError(t, err)
			priority, err := svc.CalculateCostWithServiceTier(model, tokens, 1, "priority")
			require.NoError(t, err)
			require.InDelta(t, standard.TotalCost, priority.TotalCost, 1e-12)
		})
	}
}

// 3.8 Flash 现行价卡是 promotional 价，2027-01-01 起官方翻倍。到期这天此用例会红，
// 提醒同时更新 fallbackPrices 与价格目录——注释里写着的到期日不会提醒任何人。
func TestAntigravityGeminiFlash_IntroductoryPricingNotExpired(t *testing.T) {
	expiry := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if !time.Now().UTC().Before(expiry) {
		t.Fatal("gemini-3.8-flash 的 promotional 价已到期，请同步更新 fallbackPrices 与 model_prices_and_context_window.json")
	}
}
