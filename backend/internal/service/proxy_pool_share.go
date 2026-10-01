package service

import (
	"fmt"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	SettingKeyProxyPoolDefaultShareable = "proxy_pool_default_shareable"
	SettingKeyProxyPoolDefaultShareMax  = "proxy_pool_default_share_max"
)

var (
	ErrProxyPoolSlotNotShared = infraerrors.Conflict("PROXY_POOL_SLOT_NOT_SHARED",
		"this proxy pool slot is not shared and another account already uses it")
	ErrProxyPoolSlotFull = infraerrors.Conflict("PROXY_POOL_SLOT_FULL",
		"this proxy pool slot has reached its account limit")
	ErrProxyPoolNoCapacity = infraerrors.ServiceUnavailable("PROXY_POOL_NO_CAPACITY",
		"every proxy pool slot is taken and no shared slot has room")
	ErrProxyPoolBulkUnsupported = infraerrors.BadRequest("PROXY_POOL_BULK_UNSUPPORTED",
		"bulk edit cannot assign a proxy pool proxy")
	ErrProxyPoolShareMaxInvalid = infraerrors.BadRequest("PROXY_POOL_SHARE_MAX_INVALID",
		"pool_share_max must be >= 0")
)

// NewProxyPoolSlotFullError has ErrProxyPoolSlotFull's code and reason, so
// errors.Is matches it (ApplicationError.Is compares code and reason), and
// says how full the slot is.
func NewProxyPoolSlotFullError(used, capacity int64) error {
	return infraerrors.Conflict("PROXY_POOL_SLOT_FULL",
		fmt.Sprintf("this proxy pool slot has reached its account limit (%d / %d)", used, capacity))
}

// ProxyPoolShareCandidate is a shared pool-managed proxy that still has room.
type ProxyPoolShareCandidate struct {
	Proxy Proxy
	Used  int64
}
