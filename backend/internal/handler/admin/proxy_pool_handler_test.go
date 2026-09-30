//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeProxyPoolService struct {
	gotURL      string
	gotToken    *string
	rotatedID   int64
	leaseErr    error
	updateCalls int
}

func (f *fakeProxyPoolService) GetConfig(context.Context) (service.ProxyPoolConfigView, error) {
	return service.ProxyPoolConfigView{URL: "http://vpngate:20000", URLSource: "env", TokenSource: "env", TokenConfigured: true}, nil
}

func (f *fakeProxyPoolService) UpdateConfig(_ context.Context, rawURL string, token *string) (service.ProxyPoolConfigView, error) {
	f.updateCalls++
	f.gotURL, f.gotToken = rawURL, token
	return service.ProxyPoolConfigView{URL: rawURL, URLSource: "setting"}, nil
}

func (f *fakeProxyPoolService) Health(context.Context) (map[string]any, error) {
	return map[string]any{"status": "ok"}, nil
}

func (f *fakeProxyPoolService) Lease(context.Context) (*service.Proxy, error) {
	if f.leaseErr != nil {
		return nil, f.leaseErr
	}
	return &service.Proxy{
		ID: 42, Name: "Proxy pool · slot03", Protocol: "http", Host: "vpngate", Port: 20003,
		Username: "slot03", Password: "secret-password", Status: service.StatusActive,
		FallbackMode: service.FallbackModeNone, ManagedBy: service.ProxyManagedByPool, ExternalRef: "l-1",
	}, nil
}

func (f *fakeProxyPoolService) Rotate(_ context.Context, id int64) (string, error) {
	f.rotatedID = id
	return "VPNGate-JP-2", nil
}

func newProxyPoolRouter(f *fakeProxyPoolService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := &ProxyPoolHandler{svc: f}
	r := gin.New()
	g := r.Group("/proxies")
	// Same registration order as routes/admin.go, including the /:id routes
	// the static /pool/... paths sit next to.
	g.GET("/pool/config", h.GetConfig)
	g.PUT("/pool/config", h.UpdateConfig)
	g.GET("/pool/health", h.Health)
	g.POST("/pool/lease", h.Lease)
	g.GET("/:id", func(c *gin.Context) { c.String(http.StatusTeapot, "GetByID") })
	g.POST("/:id/pool/rotate", h.Rotate)
	return r
}

func serve(r *gin.Engine, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestProxyPoolLeaseReturnsAManagedProxyWithoutPassword(t *testing.T) {
	r := newProxyPoolRouter(&fakeProxyPoolService{})
	rec, out := serve(r, http.MethodPost, "/proxies/pool/lease", ``)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	data := out["data"].(map[string]any)
	require.Equal(t, float64(42), data["id"])
	require.Equal(t, true, data["managed"])
	require.NotContains(t, rec.Body.String(), "secret-password")
	require.NotContains(t, rec.Body.String(), "l-1", "the lease id is internal")
}

func TestProxyPoolLeaseErrorKeepsItsStatus(t *testing.T) {
	r := newProxyPoolRouter(&fakeProxyPoolService{leaseErr: service.ErrProxyPoolExhausted})
	rec, out := serve(r, http.MethodPost, "/proxies/pool/lease", ``)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "PROXY_POOL_EXHAUSTED", out["reason"])
}

func TestProxyPoolUpdateConfigDistinguishesMissingAndEmptyToken(t *testing.T) {
	for _, tc := range []struct {
		body      string
		wantToken *string
	}{
		{`{"url":"http://vpngate:20000"}`, nil},
		{`{"url":"http://vpngate:20000","token":null}`, nil},
		{`{"url":"http://vpngate:20000","token":""}`, ptr("")},
		{`{"url":"http://vpngate:20000","token":"abc"}`, ptr("abc")},
	} {
		f := &fakeProxyPoolService{}
		rec, _ := serve(newProxyPoolRouter(f), http.MethodPut, "/proxies/pool/config", tc.body)
		require.Equal(t, http.StatusOK, rec.Code, tc.body)
		require.Equal(t, "http://vpngate:20000", f.gotURL, tc.body)
		require.Equal(t, tc.wantToken, f.gotToken, tc.body)
	}
}

func TestProxyPoolRoutesDoNotShadowProxyByID(t *testing.T) {
	f := &fakeProxyPoolService{}
	r := newProxyPoolRouter(f)

	rec, out := serve(r, http.MethodGet, "/proxies/pool/config", ``)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "http://vpngate:20000", out["data"].(map[string]any)["url"])

	rec, _ = serve(r, http.MethodGet, "/proxies/7", ``)
	require.Equal(t, http.StatusTeapot, rec.Code, "GET /proxies/7 must still reach GetByID")

	rec, out = serve(r, http.MethodPost, "/proxies/7/pool/rotate", ``)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(7), f.rotatedID)
	require.Equal(t, "VPNGate-JP-2", out["data"].(map[string]any)["node"])

	rec, _ = serve(r, http.MethodPost, "/proxies/x/pool/rotate", ``)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func ptr(s string) *string { return &s }
