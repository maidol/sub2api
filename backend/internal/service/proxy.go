package service

import (
	"net"
	"net/url"
	"strconv"
	"time"
)

const (
	FallbackModeNone   = "none"
	FallbackModeProxy  = "proxy"
	FallbackModeDirect = "direct"
)

// ProxyManagedByPool marks a proxy leased from the proxy pool provider.
const ProxyManagedByPool = "pool"

type Proxy struct {
	ID             int64
	Name           string
	Protocol       string
	Host           string
	Port           int
	Username       string
	Password       string
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      *time.Time
	FallbackMode   string
	BackupProxyID  *int64
	ExpiryWarnDays int
	ManagedBy      string // "" = hand-made, ProxyManagedByPool = leased from the pool
	ExternalRef    string // provider lease ID when ManagedBy is set
	PoolShareable  bool   // pool-managed only: other accounts may share this slot
	PoolShareMax   int    // pool-managed only: max accounts when shared; 0 = no limit
}

// PoolCapacity is how many accounts a pool-managed proxy may hold: 1 when it
// is not shared, unlimited (limited=false) when shared with max 0.
func (p *Proxy) PoolCapacity() (capacity int64, limited bool) {
	if !p.PoolShareable {
		return 1, true
	}
	if p.PoolShareMax <= 0 {
		return 0, false
	}
	return int64(p.PoolShareMax), true
}

func (p *Proxy) IsActive() bool {
	return p.Status == StatusActive
}

// IsExpired 报告代理是否已过期（基于 expires_at，与 status 无关）。
func (p *Proxy) IsExpired(now time.Time) bool {
	return p.ExpiresAt != nil && !p.ExpiresAt.After(now)
}

func (p *Proxy) URL() string {
	u := &url.URL{
		Scheme: p.Protocol,
		Host:   net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
	}
	if p.Username != "" && p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u.String()
}

type ProxyWithAccountCount struct {
	Proxy
	AccountCount   int64
	PoolUsed       int64 // pool-managed only: accounts on it, spark shadows excluded
	LatencyMs      *int64
	LatencyStatus  string
	LatencyMessage string
	IPAddress      string
	Country        string
	CountryCode    string
	Region         string
	City           string
	QualityStatus  string
	QualityScore   *int
	QualityGrade   string
	QualitySummary string
	QualityChecked *int64
}

type ProxyAccountSummary struct {
	ID       int64
	Name     string
	Platform string
	Type     string
	Notes    *string
}
