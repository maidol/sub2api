//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// proxyFailClosedUpstream 记录出网次数：绑定的代理不可用时，任何请求都不能发出去。
type proxyFailClosedUpstream struct{ calls atomic.Int32 }

func (u *proxyFailClosedUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	u.calls.Add(1)
	return nil, context.DeadlineExceeded
}

func (u *proxyFailClosedUpstream) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	u.calls.Add(1)
	return nil, context.DeadlineExceeded
}

func requireProxyLookupFailed(t *testing.T, err error) {
	t.Helper()
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, GatewayFailureReason("proxy_unavailable_lookup-failed"), failover.Reason)
}

func TestGeminiModelsEndpointFailsClosedWhenBoundProxyMissing(t *testing.T) {
	upstream := &proxyFailClosedUpstream{}
	svc := newGeminiModelsEndpointsTestService(nil, upstream)
	proxyID := int64(42)
	account := &Account{ID: 261, Platform: PlatformGemini, Type: AccountTypeAPIKey, ProxyID: &proxyID, Credentials: map[string]any{
		"api_key":  "sk-pool",
		"base_url": "https://relay.example.com",
	}}

	res, err := svc.ForwardListModels(context.Background(), account)
	require.Nil(t, res)
	requireProxyLookupFailed(t, err)
	require.Zero(t, upstream.calls.Load(), "绑定的代理不可用时不得直连上游")
}

func TestGeminiCachedContentFailsClosedWhenBoundProxyMissing(t *testing.T) {
	upstream := &proxyFailClosedUpstream{}
	svc := NewGeminiCachedContentService(newGeminiCachedContentRepoStub(),
		&GeminiMessagesCompatService{httpUpstream: upstream, cfg: &config.Config{}}, &config.Config{})
	req, err := ParseGeminiCachedContentCreateRequest([]byte(`{"model":"gemini-3.8-flash"}`), time.Now(), 24*time.Hour)
	require.NoError(t, err)
	proxyID := int64(42)
	account := &Account{ID: 9, Platform: PlatformGemini, Type: AccountTypeAPIKey, ProxyID: &proxyID, Credentials: map[string]any{"api_key": "secret"}}

	_, err = svc.CreateUpstream(context.Background(), account, req, "gemini-3.8-flash", "pub1", time.Hour)
	require.ErrorIs(t, err, ErrGeminiCachedContentRequestNotSent)
	requireProxyLookupFailed(t, err)
	require.Zero(t, upstream.calls.Load(), "绑定的代理不可用时不得直连上游")
}

func TestModelProtocolCatalogFailsClosedWhenBoundProxyMissing(t *testing.T) {
	upstream := &proxyFailClosedUpstream{}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	base := fmt.Sprintf("http://cc-proxy-%d.example/provider/v1", time.Now().UnixNano())
	account := commandCodeTestAccount(15)
	account.Credentials["api_base_urls"] = map[string]any{
		APIProtocolChatCompletions: base,
		APIProtocolResponses:       base,
	}
	proxyID := int64(42)
	account.ProxyID = &proxyID

	require.Nil(t, svc.modelCatalogProtocols(t.Context(), account, "deepseek/deepseek-v4-flash"))
	require.Zero(t, upstream.calls.Load(), "绑定的代理不可用时不得直连上游拉目录")
}
