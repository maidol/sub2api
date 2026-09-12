//go:build unit

package service

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildOpenAIAutoResetUsageUpdates_ExplicitNullSecondaryWindowClearsFiveHourKeys(t *testing.T) {
	var usage OpenAIQuotaUsage
	err := json.Unmarshal([]byte(`{
		"rate_limit": {
			"primary_window": {
				"used_percent": 49,
				"limit_window_seconds": 2592000,
				"reset_after_seconds": 2428562
			},
			"secondary_window": null
		}
	}`), &usage)
	if err != nil {
		t.Fatal(err)
	}

	updates := buildOpenAIAutoResetUsageUpdates(&usage, time.Date(2026, 9, 11, 7, 55, 32, 0, time.UTC))
	for _, key := range []string{
		"codex_5h_used_percent",
		"codex_5h_reset_after_seconds",
		"codex_5h_window_minutes",
		"codex_5h_reset_at",
	} {
		value, ok := updates[key]
		if !ok || value != nil {
			t.Fatalf("updates[%q] = %v (present=%v), want nil deletion marker", key, value, ok)
		}
	}
	if got := updates["codex_7d_used_percent"]; got != 49.0 {
		t.Fatalf("codex_7d_used_percent = %v, want 49", got)
	}
}
