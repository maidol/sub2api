package service

import (
	"context"
	"strings"
	"time"
)

const gemini38SchedulerModel = "gemini-3.8-flash-tiered"

type Gemini38SchedulerStatus struct {
	Supported     bool       `json:"supported"`
	Schedulable   bool       `json:"schedulable"`
	RateLimited   bool       `json:"rate_limited"`
	UsingOverages bool       `json:"using_overages"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	Reason        string     `json:"reason,omitempty"`
}

func normalizeAntigravityModelName(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.LastIndex(normalized, "/publishers/google/models/"); idx != -1 {
		normalized = normalized[idx+len("/publishers/google/models/"):]
	} else if idx := strings.LastIndex(normalized, "/publishers/anthropic/models/"); idx != -1 {
		normalized = normalized[idx+len("/publishers/anthropic/models/"):]
	} else if idx := strings.LastIndex(normalized, "/models/"); idx != -1 {
		normalized = normalized[idx+len("/models/"):]
	} else {
		normalized = strings.TrimPrefix(normalized, "publishers/google/models/")
		normalized = strings.TrimPrefix(normalized, "publishers/anthropic/models/")
		normalized = strings.TrimPrefix(normalized, "models/")
	}
	return normalized
}

// resolveAntigravityModelKey 根据请求的模型名解析限流 key
// 返回空字符串表示无法解析
func resolveAntigravityModelKey(requestedModel string) string {
	return normalizeAntigravityModelName(requestedModel)
}

// IsSchedulableForModel 结合模型级限流判断是否可调度。
// 保持旧签名以兼容既有调用方；默认使用 context.Background()。
func (a *Account) IsSchedulableForModel(requestedModel string) bool {
	return a.IsSchedulableForModelWithContext(context.Background(), requestedModel)
}

// EvaluateGemini38SchedulerStatus returns the current read-only routing status
// for the canonical Gemini 3.8 Flash request model. It reuses the same mapping,
// family-scope and overages rules as request scheduling.
func (a *Account) EvaluateGemini38SchedulerStatus(ctx context.Context) Gemini38SchedulerStatus {
	status := Gemini38SchedulerStatus{}
	if a == nil || a.Platform != PlatformAntigravity {
		status.Reason = "unsupported"
		return status
	}
	if !a.IsModelSupported(gemini38SchedulerModel) {
		status.Reason = "unsupported"
		return status
	}
	status.Supported = true
	status.Schedulable = a.IsSchedulableForModelWithContext(ctx, gemini38SchedulerModel)
	status.RateLimited = a.isModelRateLimitedWithContext(ctx, gemini38SchedulerModel)
	if status.RateLimited {
		status.ResetAt = a.gemini38SchedulerResetAt(ctx)
	}
	if status.RateLimited {
		if a.IsOveragesEnabled() && !a.isCreditsExhausted() && status.Schedulable {
			status.UsingOverages = true
			status.Reason = "overage_available"
		} else if a.isCreditsExhausted() {
			status.Reason = "credits_exhausted"
		} else {
			status.Reason = "model_rate_limited"
		}
	} else if !status.Schedulable {
		status.Reason = "account_unschedulable"
	} else {
		status.Reason = "schedulable"
	}
	return status
}

func (a *Account) gemini38SchedulerResetAt(ctx context.Context) *time.Time {
	var resetAt *time.Time
	for _, key := range a.modelRateLimitKeysForRequest(ctx, gemini38SchedulerModel) {
		candidate := a.modelRateLimitResetAt(key)
		if candidate == nil || !time.Now().Before(*candidate) {
			continue
		}
		if resetAt == nil || candidate.After(*resetAt) {
			resetAt = candidate
		}
	}
	if creditsResetAt := a.modelRateLimitResetAt(creditsExhaustedKey); creditsResetAt != nil && time.Now().Before(*creditsResetAt) {
		if resetAt == nil || creditsResetAt.After(*resetAt) {
			resetAt = creditsResetAt
		}
	}
	return resetAt
}

func (a *Account) IsSchedulableForModelWithContext(ctx context.Context, requestedModel string) bool {
	if a == nil {
		return false
	}
	if !a.IsSchedulable() {
		return false
	}
	if a.isModelRateLimitedWithContext(ctx, requestedModel) {
		// Antigravity + overages 启用 + 积分未耗尽 → 放行（有积分可用）
		if a.Platform == PlatformAntigravity && a.IsOveragesEnabled() && !a.isCreditsExhausted() {
			return true
		}
		return false
	}
	return true
}

// GetRateLimitRemainingTime 获取限流剩余时间（模型级限流）
// 返回 0 表示未限流或已过期
func (a *Account) GetRateLimitRemainingTime(requestedModel string) time.Duration {
	return a.GetRateLimitRemainingTimeWithContext(context.Background(), requestedModel)
}

// GetRateLimitRemainingTimeWithContext 获取限流剩余时间（模型级限流）
// 返回 0 表示未限流或已过期
func (a *Account) GetRateLimitRemainingTimeWithContext(ctx context.Context, requestedModel string) time.Duration {
	if a == nil {
		return 0
	}
	return a.GetModelRateLimitRemainingTimeWithContext(ctx, requestedModel)
}
