//go:build e2e

// 运行说明：
// 在本机开发环境中，sub2api-app 容器端口通过 docker bridge 访问。
// 运行示例：
//   BASE_URL=http://172.17.0.8:8080 go test -tags=e2e -count=1 -v ./internal/integration/ -run TestAntigravityUsageE2E
package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func loginWithAPIV1(t *testing.T) string {
	t.Helper()
	adminEmail := getEnv("ADMIN_EMAIL", "admin@sub2api.local")
	adminPassword := getEnv("ADMIN_PASSWORD", "")
	if adminPassword == "" {
		cmd := exec.Command("docker", "inspect", "sub2api-app")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("docker inspect error: %v, out: %s", err, string(out))
		} else {
			t.Logf("docker inspect succeeded, length: %d", len(out))
			var containers []struct {
				Config struct {
					Env []string `json:"Env"`
				} `json:"Config"`
			}
			if err := json.Unmarshal(out, &containers); err == nil && len(containers) > 0 {
				t.Logf("Env count: %d", len(containers[0].Config.Env))
				for _, s := range containers[0].Config.Env {
					if strings.Contains(s, "PASSWORD") || strings.Contains(s, "ADMIN") {
						t.Logf("Env item: %s", strings.Split(s, "=")[0])
					}
					if strings.HasPrefix(strings.TrimSpace(s), "ADMIN_PASSWORD=") {
						adminPassword = strings.TrimPrefix(strings.TrimSpace(s), "ADMIN_PASSWORD=")
						t.Logf("Found ADMIN_PASSWORD in container Env!")
						break
					}
				}
			} else {
				t.Logf("Unmarshal error: %v", err)
			}
		}
	}
	if adminPassword == "" {
		t.Logf("adminPassword is empty!")
		return ""
	}

	payload := map[string]string{
		"email":    adminEmail,
		"password": adminPassword,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", baseURL+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		t.Logf("NewRequest err: %v", err)
		return ""
	}
	req.Header.Set("Content-Type", "application/json")

	transport := &http.Transport{
		Proxy: nil, // 禁用代理直连本地 docker bridge
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Logf("client.Do err: %v", err)
		return ""
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Logf("login status %d, body: %s", resp.StatusCode, string(respBody))
		return ""
	}
	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return ""
	}

	if data, ok := result["data"].(map[string]any); ok {
		if token, ok := data["access_token"].(string); ok {
			return token
		}
	}
	return ""
}

func TestAntigravityUsageE2E(t *testing.T) {
	token := loginWithAPIV1(t)
	if token == "" {
		t.Fatal("无法登录获取 token")
		return
	}

	url := baseURL + "/api/v1/admin/accounts/1/usage"
	req, err := http.NewRequest("GET", url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)

	transport := &http.Transport{
		Proxy: nil, // 禁用代理直连本地 docker bridge
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var res struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AntigravityQuota      map[string]any   `json:"antigravity_quota"`
			AntigravityPools      []map[string]any `json:"antigravity_pools"`
			AntigravityPoolSource string           `json:"antigravity_pool_source"`
		} `json:"data"`
	}

	err = json.Unmarshal(body, &res)
	require.NoError(t, err)

	t.Logf("Response Code: %d, Message: %s", res.Code, res.Message)
	t.Logf("AntigravityPoolSource: %s", res.Data.AntigravityPoolSource)
	t.Logf("AntigravityQuota keys count: %d", len(res.Data.AntigravityQuota))
	t.Logf("AntigravityPools count: %d", len(res.Data.AntigravityPools))

	for i, pool := range res.Data.AntigravityPools {
		t.Logf("Pool #%d: %+v", i, pool)
	}

	// 核心断言：不能在 pools 为空时通过，必须非空
	require.NotEmpty(t, res.Data.AntigravityPools, "antigravity_pools 必须存在且非空（上游返回 quota keys: %d）", len(res.Data.AntigravityQuota))

	// 来源断言：在权威接口调通时应为 quota_summary
	if res.Data.AntigravityPoolSource != "" {
		require.Contains(t, []string{"quota_summary", "per_model_inferred"}, res.Data.AntigravityPoolSource)
	}

	var resetsAts []string
	var weeklyCount int
	for _, p := range res.Data.AntigravityPools {
		poolName, ok := p["pool"].(string)
		require.True(t, ok)
		require.Contains(t, []string{"gemini", "claude_gpt"}, poolName)

		// FiveHour 必须存在
		fh, ok := p["five_hour"].(map[string]any)
		require.True(t, ok, "pool %s 的 five_hour 必须非空", poolName)
		require.Contains(t, fh, "utilization")

		// Weekly 检查
		if w, ok := p["weekly"].(map[string]any); ok && w != nil {
			weeklyCount++
			require.Contains(t, w, "utilization")
			t.Logf("Pool %s Weekly utilization: %v", poolName, w["utilization"])
		}

		// 检查 resets_at 与 remaining_seconds 自洽性
		if resetStr, ok := fh["resets_at"].(string); ok && resetStr != "" {
			resetsAts = append(resetsAts, resetStr)
			resetTime, err := time.Parse(time.RFC3339, resetStr)
			require.NoError(t, err, "resets_at 必须是 RFC3339 格式: %s", resetStr)

			if remSec, ok := fh["remaining_seconds"].(float64); ok {
				expectedRem := int(time.Until(resetTime).Seconds())
				if expectedRem < 0 {
					expectedRem = 0
				}
				// 允许网络和执行延迟在 30 秒以内
				require.InDelta(t, expectedRem, int(remSec), 30, "pool %s five_hour remaining_seconds 与 resets_at 必须自洽", poolName)
			}
		}

		// models 列表：在 per_model_inferred 模式下有 models
		if models, ok := p["models"].([]any); ok && len(models) > 0 {
			t.Logf("pool %s models count: %d", poolName, len(models))
		}
	}

	if res.Data.AntigravityPoolSource == "quota_summary" {
		require.Greater(t, weeklyCount, 0, "使用 quota_summary 时至少一个池有 weekly 窗口")
	}

	// 若两个池同时存在，记录两个家族池独立重置时刻
	if len(res.Data.AntigravityPools) >= 2 && len(resetsAts) >= 2 {
		t.Logf("Pool resets_at: pool0=%s, pool1=%s", resetsAts[0], resetsAts[1])
	}
}
