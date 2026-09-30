package admin

import (
	"context"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// proxyPoolService is what ProxyPoolHandler needs from service.ProxyPoolService.
type proxyPoolService interface {
	GetConfig(ctx context.Context) (service.ProxyPoolConfigView, error)
	UpdateConfig(ctx context.Context, rawURL string, token *string) (service.ProxyPoolConfigView, error)
	Health(ctx context.Context) (map[string]any, error)
	Lease(ctx context.Context) (*service.Proxy, error)
	Rotate(ctx context.Context, proxyID int64) (string, error)
}

// ProxyPoolHandler serves proxy pool mode: provider settings, leasing a
// pool-managed proxy for an account, and rotating its exit node.
type ProxyPoolHandler struct {
	svc proxyPoolService
}

func NewProxyPoolHandler(svc *service.ProxyPoolService) *ProxyPoolHandler {
	return &ProxyPoolHandler{svc: svc}
}

// GetConfig GET /api/v1/admin/proxies/pool/config
func (h *ProxyPoolHandler) GetConfig(c *gin.Context) {
	view, err := h.svc.GetConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, view)
}

// UpdateProxyPoolConfigRequest: token absent or null keeps the saved token,
// "" removes it; url "" removes the saved URL (the environment variable
// applies again).
type UpdateProxyPoolConfigRequest struct {
	URL   string  `json:"url"`
	Token *string `json:"token"`
}

// UpdateConfig PUT /api/v1/admin/proxies/pool/config
func (h *ProxyPoolHandler) UpdateConfig(c *gin.Context) {
	var req UpdateProxyPoolConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	view, err := h.svc.UpdateConfig(c.Request.Context(), req.URL, req.Token)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, view)
}

// Health GET /api/v1/admin/proxies/pool/health
func (h *ProxyPoolHandler) Health(c *gin.Context) {
	out, err := h.svc.Health(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, out)
}

// Lease POST /api/v1/admin/proxies/pool/lease — leases a proxy for one
// account; the caller puts the returned id into the account's proxy_id.
func (h *ProxyPoolHandler) Lease(c *gin.Context) {
	proxy, err := h.svc.Lease(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.ProxyFromService(proxy))
}

// Rotate POST /api/v1/admin/proxies/:id/pool/rotate
func (h *ProxyPoolHandler) Rotate(c *gin.Context) {
	proxyID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid proxy ID")
		return
	}
	node, err := h.svc.Rotate(c.Request.Context(), proxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"node": node})
}
