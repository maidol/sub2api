//go:build e2e

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
			AntigravityQuota map[string]any   `json:"antigravity_quota"`
			AntigravityPools []map[string]any `json:"antigravity_pools"`
		} `json:"data"`
	}

	err = json.Unmarshal(body, &res)
	require.NoError(t, err)

	t.Logf("Response Code: %d, Message: %s", res.Code, res.Message)
	t.Logf("AntigravityQuota keys count: %d", len(res.Data.AntigravityQuota))
	t.Logf("AntigravityPools count: %d", len(res.Data.AntigravityPools))

	for i, pool := range res.Data.AntigravityPools {
		t.Logf("Pool #%d: %+v", i, pool)
	}

	// 验证结构体中 AntigravityPools 的解析契约
	if len(res.Data.AntigravityPools) > 0 {
		for _, p := range res.Data.AntigravityPools {
			poolName, ok := p["pool"].(string)
			require.True(t, ok)
			require.Contains(t, []string{"gemini", "claude_gpt"}, poolName)

			if fh, ok := p["five_hour"].(map[string]any); ok && fh != nil {
				require.Contains(t, fh, "utilization")
				t.Logf("Pool %s FiveHour utilization: %v", poolName, fh["utilization"])
			}
		}
	} else {
		t.Logf("antigravity_pools 为空，说明上游模型额度不齐平或无数据，触发正常回落到逐模型清单")
	}
}
