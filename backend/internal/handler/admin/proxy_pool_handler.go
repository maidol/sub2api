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
	UpdateShareDefaults(ctx context.Context, shareable *bool, shareMax *int) error
	Health(ctx context.Context) (map[string]any, error)
	Allocate(ctx context.Context) (*service.Proxy, bool, error)
	ListShareable(ctx context.Context) ([]service.ProxyPoolShareCandidate, error)
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
	// Share settings for newly leased slots; absent keeps the saved value.
	DefaultShareable *bool `json:"default_shareable"`
	DefaultShareMax  *int  `json:"default_share_max"`
}

// UpdateConfig PUT /api/v1/admin/proxies/pool/config
func (h *ProxyPoolHandler) UpdateConfig(c *gin.Context) {
	var req UpdateProxyPoolConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.svc.UpdateShareDefaults(c.Request.Context(), req.DefaultShareable, req.DefaultShareMax); err != nil {
		response.ErrorFrom(c, err)
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

// Lease POST /api/v1/admin/proxies/pool/lease — gives one account a pool
// proxy: a new slot, or a shared one when the pool is full (shared=true). The
// caller puts the returned id into the account's proxy_id.
func (h *ProxyPoolHandler) Lease(c *gin.Context) {
	proxy, shared, err := h.svc.Allocate(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.ProxyPoolAllocation{Proxy: *dto.ProxyFromService(proxy), Shared: shared})
}

// ListShareable GET /api/v1/admin/proxies/pool/shareable — shared pool slots
// that still have room, least used first.
func (h *ProxyPoolHandler) ListShareable(c *gin.Context) {
	items, err := h.svc.ListShareable(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]dto.ProxyPoolShareCandidate, 0, len(items))
	for i := range items {
		out = append(out, dto.ProxyPoolShareCandidate{
			Proxy: *dto.ProxyFromService(&items[i].Proxy),
			Used:  items[i].Used,
			Max:   items[i].Proxy.PoolShareMax,
		})
	}
	response.Success(c, out)
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
