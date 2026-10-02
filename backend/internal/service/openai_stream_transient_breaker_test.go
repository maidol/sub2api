package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 流内 response.failed / error 帧承载的 5xx 必须和非流 HTTP、WS 一样进入
// 「账号+模型」短期熔断；OAuth 账号同样适用。

const streamBreakerServerErrorFailed = `{"type":"response.failed","response":{"id":"resp_sb","status":"failed","error":{"code":"server_error","message":"An error occurred while processing your request."}}}`

func streamBreakerSSE(afterOutput bool, frames ...string) string {
	sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_sb\"}}\n\n"
	if afterOutput {
		sse += "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	}
	for _, frame := range frames {
		sse += "data: " + frame + "\n\n"
	}
	return sse
}

func newStreamBreakerService() *OpenAIGatewayService {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	svc.rateLimitService = NewRateLimitService(transientCooldownAccountRepo{}, nil, &config.Config{}, nil, nil)
	return svc
}

func runStreamBreakerStream(t *testing.T, svc *OpenAIGatewayService, account *Account, passthrough bool, sse string) error {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(sse))}
	if passthrough {
		_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.5", "gpt-5.5")
		return err
	}
	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.5", "gpt-5.5")
	return err
}

// T1：未输出前的 server_error failover，连续两次 → 账号+模型被屏蔽（API Key 与 OAuth，native 与 passthrough）。
func TestOpenAIStreamServerErrorFailoverBlocksAccountModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, passthrough := range []bool{false, true} {
			name := accountType + "/native"
			if passthrough {
				name = accountType + "/passthrough"
			}
			t.Run(name, func(t *testing.T) {
				svc := newStreamBreakerService()
				account := &Account{ID: 8101, Platform: PlatformOpenAI, Type: accountType, Status: StatusActive, Schedulable: true}
				for i := 0; i < 2; i++ {
					err := runStreamBreakerStream(t, svc, account, passthrough, streamBreakerSSE(false, streamBreakerServerErrorFailed))
					var failoverErr *UpstreamFailoverError
					require.ErrorAs(t, err, &failoverErr)
					require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
				}
				require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.5"))
				require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-luna"))
			})
		}
	}
}

// T2：WS 终止事件的 server_error 对 OAuth 账号同样计入。
func TestOpenAIWSTerminalServerErrorBlocksOAuthAccountModel(t *testing.T) {
	svc := newStreamBreakerService()
	account := &Account{ID: 8102, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for i := 0; i < 2; i++ {
		svc.handleOpenAIWSTerminalTransientFailure(context.Background(), account, "gpt-5.5", http.Header{}, []byte(streamBreakerServerErrorFailed))
	}
	require.True(t, svc.isOpenAIAccountModelRuntimeBlocked(account, "gpt-5.5"))
}

type streamBreakerFailedUpstream struct{}

func (streamBreakerFailedUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(streamBreakerSSE(false, streamBreakerServerErrorFailed))),
		Request:    req,
	}, nil
}

func (u streamBreakerFailedUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

// T3：沿真实 Forward 记失败，用调度器的同一条模型链查。OAuth 的 gpt-5.1 调度键是 gpt-5.4，
// 记和查必须落在同一个键上，否则熔断写进去了却永远查不到。
func TestOpenAIForwardStreamServerErrorBlocksSchedulerModelKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		accountType string
		model       string
	}{
		{AccountTypeOAuth, "gpt-5.1"},
		{AccountTypeOAuth, "gpt-6-luna"},
		{AccountTypeAPIKey, "gpt-5.1"},
	}
	for _, tc := range cases {
		t.Run(tc.accountType+"/"+tc.model, func(t *testing.T) {
			svc := newStreamBreakerService()
			svc.httpUpstream = streamBreakerFailedUpstream{}
			credentials := map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
			if tc.accountType == AccountTypeAPIKey {
				credentials = map[string]any{"api_key": "sk-test"}
			}
			account := &Account{ID: 8103, Name: "breaker", Platform: PlatformOpenAI, Type: tc.accountType, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: credentials}
			body := []byte(`{"model":"` + tc.model + `","stream":true,"input":"hello"}`)
			for i := 0; i < 2; i++ {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				_, err := svc.Forward(context.Background(), c, account, body)
				var failoverErr *UpstreamFailoverError
				require.True(t, errors.As(err, &failoverErr), "want failover error, got %v", err)
			}
			require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, tc.model))
		})
	}
}

// T4：请求级错误、池模式可重试状态、非 OpenAI 平台都不得进入熔断。
// 帧放在已输出之后：那条路径对每个终止事件都执行账号副作用，用例才不空转。
func TestOpenAIStreamRequestScopedFailuresDoNotBlockAccountModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	frames := map[string]string{
		"capacity_shed":   `{"type":"response.failed","response":{"id":"resp_sb","status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`,
		"context_window":  `{"type":"response.failed","response":{"id":"resp_sb","status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}}`,
		"invalid_request": `{"type":"response.failed","response":{"id":"resp_sb","status":"failed","error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid value for 'input'."}}}`,
		"safety_rejected": `{"type":"response.failed","response":{"id":"resp_sb","status":"failed","error":{"code":"image_generation_user_error","message":"Your request was rejected by the safety system."}}}`,
	}
	for name, frame := range frames {
		for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
			for _, passthrough := range []bool{false, true} {
				mode := "native"
				if passthrough {
					mode = "passthrough"
				}
				t.Run(name+"/"+accountType+"/"+mode, func(t *testing.T) {
					svc := newStreamBreakerService()
					account := &Account{ID: 8104, Platform: PlatformOpenAI, Type: accountType, Status: StatusActive, Schedulable: true}
					for i := 0; i < 3; i++ {
						_ = runStreamBreakerStream(t, svc, account, passthrough, streamBreakerSSE(true, frame))
					}
					require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.5"))
				})
			}
		}
	}

	t.Run("pool_mode_retryable_status", func(t *testing.T) {
		svc := newStreamBreakerService()
		account := &Account{
			ID: 8105, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
			Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(http.StatusInternalServerError)}},
		}
		for i := 0; i < 3; i++ {
			_ = runStreamBreakerStream(t, svc, account, true, streamBreakerSSE(true, streamBreakerServerErrorFailed))
		}
		require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.5"))
	})

	t.Run("grok_platform", func(t *testing.T) {
		svc := newStreamBreakerService()
		account := &Account{ID: 8106, Platform: PlatformGrok, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
		for i := 0; i < 3; i++ {
			svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusBadGateway, http.Header{}, []byte(`{"error":{"message":"bad gateway"}}`), "grok-4")
		}
		require.False(t, svc.isOpenAIAccountModelRuntimeBlocked(account, "grok-4"))
	})
}

// T5：OAuth 账号被屏蔽后，一次成功即解除。
func TestOpenAIOAuthModelTransientBlockClearsOnSuccess(t *testing.T) {
	svc := newStreamBreakerService()
	account := &Account{ID: 8107, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for i := 0; i < 2; i++ {
		svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusBadGateway, http.Header{}, []byte(`{"error":{"message":"bad gateway"}}`), "gpt-5.5")
	}
	require.True(t, svc.isOpenAIAccountModelRuntimeBlocked(account, "gpt-5.5"))

	svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.5", true, nil)

	require.False(t, svc.isOpenAIAccountModelRuntimeBlocked(account, "gpt-5.5"))
}

func newStreamBreakerStickyService(groupID int64) (*OpenAIGatewayService, *schedulerTestGatewayCache, Account, Account) {
	sticky := Account{ID: 8201, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}}
	other := Account{ID: 8202, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session_hash_breaker": sticky.ID}}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{sticky, other}},
		cache:              cache,
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	return svc, cache, sticky, other
}

func selectStreamBreakerAccount(t *testing.T, svc *OpenAIGatewayService, groupID int64, model string) int64 {
	t.Helper()
	selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "session_hash_breaker", model, nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	return selection.Account.ID
}

// T6：粘性账号对某模型被屏蔽 → 该模型选到别的账号并改绑；其他模型仍走粘性账号。
func TestOpenAIStickySessionYieldsOnlyForBlockedModel(t *testing.T) {
	groupID := int64(10)

	svc, cache, sticky, other := newStreamBreakerStickyService(groupID)
	for i := 0; i < 2; i++ {
		svc.handleOpenAIAccountUpstreamError(context.Background(), &sticky, http.StatusBadGateway, http.Header{}, []byte(`{"error":{"message":"bad gateway"}}`), "gpt-5.5")
	}
	require.Equal(t, other.ID, selectStreamBreakerAccount(t, svc, groupID, "gpt-5.5"))
	require.Equal(t, other.ID, cache.sessionBindings["openai:session_hash_breaker"])

	svc, _, sticky, _ = newStreamBreakerStickyService(groupID)
	for i := 0; i < 2; i++ {
		svc.handleOpenAIAccountUpstreamError(context.Background(), &sticky, http.StatusBadGateway, http.Header{}, []byte(`{"error":{"message":"bad gateway"}}`), "gpt-5.5")
	}
	require.Equal(t, sticky.ID, selectStreamBreakerAccount(t, svc, groupID, "gpt-6-luna"))
}

// T7：已输出后出现「裸 error + response.failed」一对，只算一次失败；第二次同样的流才达到屏蔽阈值。
func TestOpenAIStreamFailurePairAfterOutputCountsOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bareError := `{"type":"error","error":{"type":"server_error","code":"server_error","message":"An error occurred while processing your request."}}`
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, passthrough := range []bool{false, true} {
			name := accountType + "/native"
			if passthrough {
				name = accountType + "/passthrough"
			}
			t.Run(name, func(t *testing.T) {
				svc := newStreamBreakerService()
				account := &Account{ID: 8108, Platform: PlatformOpenAI, Type: accountType, Status: StatusActive, Schedulable: true}
				sse := streamBreakerSSE(true, bareError, streamBreakerServerErrorFailed)

				_ = runStreamBreakerStream(t, svc, account, passthrough, sse)
				require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.5"), "one upstream failure must not reach the block threshold")

				_ = runStreamBreakerStream(t, svc, account, passthrough, sse)
				require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.5"), "failures after output must still be counted")
			})
		}
	}
}
