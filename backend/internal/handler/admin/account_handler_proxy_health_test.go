package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type accountProxyHealthAdminService struct {
	*stubAdminService
	results                  map[int64]*service.ProxyLatencyInfo
	getProxyLatenciesErr     error
	getProxyLatenciesCalls   int
	lastGetProxyLatenciesIDs []int64
}

func (s *accountProxyHealthAdminService) GetProxyLatencies(_ context.Context, ids []int64) (map[int64]*service.ProxyLatencyInfo, error) {
	s.getProxyLatenciesCalls++
	s.lastGetProxyLatenciesIDs = append([]int64(nil), ids...)
	return s.results, s.getProxyLatenciesErr
}

func setupAccountListProxyHealthRouter(adminSvc *accountProxyHealthAdminService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router.GET("/api/v1/admin/accounts", handler.List)
	router.GET("/api/v1/admin/accounts/:id", handler.GetByID)
	return router
}

func newAccountListProxyHealthService() *accountProxyHealthAdminService {
	stub := newStubAdminService()
	stub.accounts = []service.Account{{
		ID: 501, Name: "proxy-health-account", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, ProxyID: new(int64(91)),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	return &accountProxyHealthAdminService{
		stubAdminService: stub,
		results: map[int64]*service.ProxyLatencyInfo{
			91: {
				Success:   false,
				Message:   "connection refused",
				UpdatedAt: time.Unix(1791201600, 0).UTC(),
			},
		},
	}
}

func TestAccountHandlerListFullIncludesProxyHealth(t *testing.T) {
	router := setupAccountListProxyHealthRouter(newAccountListProxyHealthService())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)
	proxyHealth, ok := payload.Data.Items[0]["proxy_health"].(map[string]any)
	require.True(t, ok, "proxy_health should be included in the full account response")
	require.Equal(t, "failed", proxyHealth["latency_status"])
	require.Equal(t, "connection refused", proxyHealth["latency_message"])
	require.Equal(t, float64(1791201600), proxyHealth["checked_at"])
}

func TestAccountHandlerListLiteIncludesProxyHealth(t *testing.T) {
	router := setupAccountListProxyHealthRouter(newAccountListProxyHealthService())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)
	proxyHealth, ok := payload.Data.Items[0]["proxy_health"].(map[string]any)
	require.True(t, ok, "proxy_health should be included in the lite account response")
	require.Equal(t, "failed", proxyHealth["latency_status"])
	require.Equal(t, "connection refused", proxyHealth["latency_message"])
	require.Equal(t, float64(1791201600), proxyHealth["checked_at"])
}

func TestAccountHandlerListProxyHealthChangeInvalidatesETag(t *testing.T) {
	adminSvc := newAccountListProxyHealthService()
	router := setupAccountListProxyHealthRouter(adminSvc)
	url := "/api/v1/admin/accounts?page=1&page_size=20&lite=1"

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, url, nil))
	require.Equal(t, http.StatusOK, first.Code)
	firstETag := first.Header().Get("ETag")
	require.NotEmpty(t, firstETag)

	adminSvc.results = map[int64]*service.ProxyLatencyInfo{
		91: {
			Success:   true,
			Message:   "connection refused",
			UpdatedAt: time.Unix(1791201600, 0).UTC(),
		},
	}
	second := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodGet, url, nil)
	secondRequest.Header.Set("If-None-Match", firstETag)
	router.ServeHTTP(second, secondRequest)

	require.Equal(t, http.StatusOK, second.Code, "changed proxy health must not return the cached 304 response")
	require.NotEqual(t, firstETag, second.Header().Get("ETag"))

	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)
	proxyHealth, ok := payload.Data.Items[0]["proxy_health"].(map[string]any)
	require.True(t, ok, "proxy_health should be included in the refreshed response")
	require.Equal(t, "success", proxyHealth["latency_status"])
}

func TestAccountHandlerListProxyHealthBatchesUniqueProxyIDs(t *testing.T) {
	adminSvc := newAccountListProxyHealthService()
	sharedProxyID := int64(91)
	adminSvc.accounts = []service.Account{
		{
			ID: 501, Name: "first-account", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, ProxyID: new(sharedProxyID),
		},
		{
			ID: 502, Name: "second-account", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, ProxyID: new(sharedProxyID),
		},
	}
	adminSvc.results = map[int64]*service.ProxyLatencyInfo{
		91: {
			Success:   true,
			Message:   "healthy",
			UpdatedAt: time.Unix(1791201600, 0).UTC(),
		},
	}
	router := setupAccountListProxyHealthRouter(adminSvc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, adminSvc.getProxyLatenciesCalls)
	require.Equal(t, []int64{91}, adminSvc.lastGetProxyLatenciesIDs)
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 2)
	for _, item := range payload.Data.Items {
		proxyHealth, ok := item["proxy_health"].(map[string]any)
		require.True(t, ok, "proxy_health should be included for accounts sharing a proxy")
		require.Equal(t, "success", proxyHealth["latency_status"])
	}
}

func TestAccountHandlerListProxyHealthMapsEachProxyToItsAccount(t *testing.T) {
	adminSvc := newAccountListProxyHealthService()
	firstProxyID, secondProxyID := int64(91), int64(92)
	adminSvc.accounts = []service.Account{
		{
			ID: 501, Name: "first-account", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, ProxyID: &firstProxyID,
		},
		{
			ID: 502, Name: "second-account", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, ProxyID: &secondProxyID,
		},
	}
	adminSvc.results = map[int64]*service.ProxyLatencyInfo{
		91: {
			Success:   false,
			Message:   "first proxy failed",
			UpdatedAt: time.Unix(1791201601, 0).UTC(),
		},
		92: {
			Success:   true,
			Message:   "second proxy healthy",
			UpdatedAt: time.Unix(1791201602, 0).UTC(),
		},
	}
	router := setupAccountListProxyHealthRouter(adminSvc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, adminSvc.getProxyLatenciesCalls)
	require.Equal(t, []int64{91, 92}, adminSvc.lastGetProxyLatenciesIDs)
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 2)
	firstHealth, ok := payload.Data.Items[0]["proxy_health"].(map[string]any)
	require.True(t, ok, "first account proxy health should be present")
	require.Equal(t, "failed", firstHealth["latency_status"])
	require.Equal(t, "first proxy failed", firstHealth["latency_message"])
	secondHealth, ok := payload.Data.Items[1]["proxy_health"].(map[string]any)
	require.True(t, ok, "second account proxy health should be present")
	require.Equal(t, "success", secondHealth["latency_status"])
	require.Equal(t, "second proxy healthy", secondHealth["latency_message"])
}

func TestAccountHandlerListOmitsProxyHealthWithoutProxyOrCacheRecord(t *testing.T) {
	adminSvc := newAccountListProxyHealthService()
	proxyIDWithoutCacheRecord := int64(92)
	adminSvc.accounts = []service.Account{
		{
			ID: 501, Name: "no-proxy-account", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive,
		},
		{
			ID: 502, Name: "uncached-proxy-account", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, ProxyID: &proxyIDWithoutCacheRecord,
		},
	}
	adminSvc.results = map[int64]*service.ProxyLatencyInfo{}
	router := setupAccountListProxyHealthRouter(adminSvc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, adminSvc.getProxyLatenciesCalls)
	require.Equal(t, []int64{92}, adminSvc.lastGetProxyLatenciesIDs)
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 2)
	for _, item := range payload.Data.Items {
		_, exists := item["proxy_health"]
		require.False(t, exists, "proxy_health should be omitted without a proxy latency cache record")
	}
}

func TestAccountHandlerListProxyHealthCacheErrorOmitsAllHealth(t *testing.T) {
	adminSvc := newAccountListProxyHealthService()
	adminSvc.getProxyLatenciesErr = context.DeadlineExceeded
	router := setupAccountListProxyHealthRouter(adminSvc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, adminSvc.getProxyLatenciesCalls)
	require.Equal(t, []int64{91}, adminSvc.lastGetProxyLatenciesIDs)
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)
	for _, item := range payload.Data.Items {
		_, exists := item["proxy_health"]
		require.False(t, exists, "proxy_health should be omitted when the cache lookup fails")
	}
}

func TestAccountHandlerGetByIDOmitsProxyHealth(t *testing.T) {
	adminSvc := newAccountListProxyHealthService()
	proxyID := int64(91)
	adminSvc.getAccountResult = &service.Account{
		ID: 501, Name: "proxy-health-account", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, ProxyID: &proxyID,
	}
	router := setupAccountListProxyHealthRouter(adminSvc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/501", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	_, exists := payload.Data["proxy_health"]
	require.False(t, exists, "single-account detail should not include proxy_health")
}
