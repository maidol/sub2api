//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// urlLevelRateLimitBody 是触发 URL 级别降级的上游报文（isURLLevelRateLimit 匹配这个形状）。
const urlLevelRateLimitBody = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

func queued429Responses(n int) []*http.Response {
	out := make([]*http.Response, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader([]byte(urlLevelRateLimitBody))),
		})
	}
	return out
}

// TestAntigravityRetryLoop_URLLevel429_RecordsFallbackLegOpsEvent
// 验证：URL 级 429 触发 daily -> prod 降级时，被降级掉的那一跳也要写进 ops 事件。
//
// 少了这条事件，ops 里就只剩最后那个 host，运维无法区分
// 「GATEWAY_ANTIGRAVITY_FORWARD_BASE_URL 没生效」和「生效了但回退到了 prod」。
func TestAntigravityRetryLoop_URLLevel429_RecordsFallbackLegOpsEvent(t *testing.T) {
	t.Setenv(antigravityForwardBaseURLEnv, "daily")

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent", nil)

	var hostsHit []string
	upstream := &queuedHTTPUpstreamStub{
		responses: queued429Responses(8),
		onCall: func(req *http.Request, _ *queuedHTTPUpstreamStub) {
			if req != nil && req.URL != nil {
				hostsHit = append(hostsHit, req.URL.Host)
			}
		},
	}

	account := &Account{
		ID:          148,
		Name:        "acc-148",
		Type:        AccountTypeOAuth,
		Platform:    PlatformAntigravity,
		Schedulable: true,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"plan_type": "free"},
	}

	svc := &AntigravityGatewayService{}
	_, _ = svc.antigravityRetryLoop(antigravityRetryLoopParams{
		ctx:          context.Background(),
		prefix:       "[test]",
		account:      account,
		accessToken:  "token",
		action:       "streamGenerateContent",
		body:         []byte(`{"input":"test"}`),
		c:            c,
		httpUpstream: upstream,
		accountRepo:  &stubAntigravityAccountRepo{},
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	})

	// 前提条件：env 生效，daily 是第一跳，并且确实回退到了 prod。
	// 这两条在修复前后都应该是绿的——它们不是本测试要钉的东西，而是让红的原因唯一。
	require.NotEmpty(t, hostsHit, "上游应被调用")
	require.Equal(t, "daily-cloudcode-pa.googleapis.com", hostsHit[0],
		"env=daily 时第一跳必须是 daily")
	require.Contains(t, hostsHit, "cloudcode-pa.googleapis.com",
		"URL 级 429 应触发到 prod 的降级")

	// 本测试要钉的：被降级掉的那一跳必须留下 ops 事件。
	var events []*OpsUpstreamErrorEvent
	if v, ok := c.Get(OpsUpstreamErrorsKey); ok {
		events, _ = v.([]*OpsUpstreamErrorEvent)
	}

	var dailyEvent *OpsUpstreamErrorEvent
	for _, ev := range events {
		u, err := url.Parse(ev.UpstreamURL)
		if err == nil && u.Host == "daily-cloudcode-pa.googleapis.com" {
			dailyEvent = ev
			break
		}
	}

	require.NotNil(t, dailyEvent,
		"URL 级 429 降级掉的 daily 那一跳必须写入 ops 事件，否则 ops 只剩 prod，无法区分 env 未生效与已回退")
	require.Equal(t, http.StatusTooManyRequests, dailyEvent.UpstreamStatusCode,
		"降级事件应带上 429 状态码")
	require.Equal(t, "url_fallback", dailyEvent.Kind,
		"降级事件的 Kind 应能与普通 retry 区分")
}
