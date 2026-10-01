package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ProxyPoolService implements proxy pool mode: it leases proxies from a pool
// provider (the vpngate sidecar's lease API, see deploy/VPNGATE.md) and stores
// each lease as a pool-managed Proxy row that one account then uses through
// the ordinary proxy_id. A lease's endpoint never changes; "rotate" only moves
// the exit node behind it, so no account ever has to be rebound.
//
// Pool-managed rows that no account uses any more are released by Reconcile,
// but only once they are proxyPoolRowGrace old: an admin may lease in the
// account form and save much later, and an account saved with the id of a row
// that was already deleted would lose its proxy.
type ProxyPoolService struct {
	settingRepo SettingRepository
	proxyRepo   ProxyRepository
	envURL      string
	envToken    string
	client      *http.Client
	now         func() time.Time
	newRef      func() string

	interval time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

const (
	SettingKeyProxyPoolURL   = "proxy_pool_url"
	SettingKeyProxyPoolToken = "proxy_pool_token"

	proxyPoolClientRefPrefix  = "sub2api-"
	proxyPoolRowGrace         = 24 * time.Hour
	proxyPoolOrphanLeaseGrace = 10 * time.Minute
	proxyPoolRequestTimeout   = 3 * time.Minute // a lease or rotate probes candidates first
)

var (
	ErrProxyPoolNotConfigured = infraerrors.BadRequest("PROXY_POOL_NOT_CONFIGURED", "proxy pool provider URL and token are not configured")
	ErrProxyNotPoolManaged    = infraerrors.BadRequest("PROXY_NOT_POOL_MANAGED", "proxy is not managed by the proxy pool")
	ErrProxyPoolExhausted     = infraerrors.ServiceUnavailable("PROXY_POOL_EXHAUSTED", "proxy pool has no free proxy")
	ErrProxyPoolNoHealthyNode = infraerrors.ServiceUnavailable("PROXY_POOL_NO_HEALTHY_NODE", "proxy pool has no healthy exit node right now")
	ErrProxyPoolLeaseGone     = infraerrors.Conflict("PROXY_POOL_LEASE_GONE", "the provider no longer knows this proxy's lease")
	ErrProxyPoolUnavailable   = infraerrors.ServiceUnavailable("PROXY_POOL_UNAVAILABLE", "proxy pool provider request failed")
	ErrProxyPoolURLInvalid    = infraerrors.BadRequest("PROXY_POOL_URL_INVALID", "proxy pool provider URL must be http(s)://host[:port]")
)

func NewProxyPoolService(settingRepo SettingRepository, proxyRepo ProxyRepository, envURL, envToken string, interval time.Duration) *ProxyPoolService {
	return &ProxyPoolService{
		settingRepo: settingRepo,
		proxyRepo:   proxyRepo,
		envURL:      strings.TrimSpace(envURL),
		envToken:    strings.TrimSpace(envToken),
		// Proxy: nil — the provider is an internal service; never route it
		// through HTTP(S)_PROXY.
		client:   &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: proxyPoolRequestTimeout},
		now:      time.Now,
		newRef:   newProxyPoolClientRef,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

func newProxyPoolClientRef() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return proxyPoolClientRefPrefix + hex.EncodeToString(b)
}

// ProxyPoolConfigView is what the admin API shows. The token is never returned.
type ProxyPoolConfigView struct {
	URL             string `json:"url"`
	URLSource       string `json:"url_source"`   // "setting", "env" or ""
	TokenSource     string `json:"token_source"` // "setting", "env" or ""
	TokenConfigured bool   `json:"token_configured"`
	// Share settings a newly leased slot starts with.
	DefaultShareable bool `json:"default_shareable"`
	DefaultShareMax  int  `json:"default_share_max"`
}

func (s *ProxyPoolService) settingValue(ctx context.Context, key string) (string, error) {
	v, err := s.settingRepo.GetValue(ctx, key)
	if errors.Is(err, ErrSettingNotFound) {
		return "", nil
	}
	return strings.TrimSpace(v), err
}

// resolve returns the effective URL and token: a value saved in the UI wins
// over the environment variable.
func (s *ProxyPoolService) resolve(ctx context.Context) (baseURL, token string, view ProxyPoolConfigView, err error) {
	settingURL, err := s.settingValue(ctx, SettingKeyProxyPoolURL)
	if err != nil {
		return "", "", view, err
	}
	settingToken, err := s.settingValue(ctx, SettingKeyProxyPoolToken)
	if err != nil {
		return "", "", view, err
	}
	switch {
	case settingURL != "":
		baseURL, view.URLSource = settingURL, "setting"
	case s.envURL != "":
		baseURL, view.URLSource = s.envURL, "env"
	}
	switch {
	case settingToken != "":
		token, view.TokenSource = settingToken, "setting"
	case s.envToken != "":
		token, view.TokenSource = s.envToken, "env"
	}
	view.URL = baseURL
	view.TokenConfigured = token != ""
	return strings.TrimRight(baseURL, "/"), token, view, nil
}

// GetConfig returns the effective configuration without the token.
func (s *ProxyPoolService) GetConfig(ctx context.Context) (ProxyPoolConfigView, error) {
	_, _, view, err := s.resolve(ctx)
	if err != nil {
		return view, err
	}
	view.DefaultShareable, view.DefaultShareMax, err = s.shareDefaults(ctx)
	return view, err
}

// shareDefaults returns the share settings a newly leased slot starts with.
func (s *ProxyPoolService) shareDefaults(ctx context.Context) (bool, int, error) {
	shareable, err := s.settingValue(ctx, SettingKeyProxyPoolDefaultShareable)
	if err != nil {
		return false, 0, err
	}
	rawMax, err := s.settingValue(ctx, SettingKeyProxyPoolDefaultShareMax)
	if err != nil {
		return false, 0, err
	}
	shareMax, _ := strconv.Atoi(rawMax)
	if shareMax < 0 {
		shareMax = 0
	}
	return shareable == "true", shareMax, nil
}

// UpdateShareDefaults saves the share settings for newly leased slots; nil
// keeps a value. Existing slots are not changed.
func (s *ProxyPoolService) UpdateShareDefaults(ctx context.Context, shareable *bool, shareMax *int) error {
	if shareMax != nil && *shareMax < 0 {
		return ErrProxyPoolShareMaxInvalid
	}
	if shareable != nil {
		if err := s.settingRepo.Set(ctx, SettingKeyProxyPoolDefaultShareable, strconv.FormatBool(*shareable)); err != nil {
			return err
		}
	}
	if shareMax != nil {
		if err := s.settingRepo.Set(ctx, SettingKeyProxyPoolDefaultShareMax, strconv.Itoa(*shareMax)); err != nil {
			return err
		}
	}
	return nil
}

// UpdateConfig saves the UI values. url == "" removes the saved URL (the
// environment variable applies again). token == nil keeps the saved token,
// "" removes it.
func (s *ProxyPoolService) UpdateConfig(ctx context.Context, rawURL string, token *string) (ProxyPoolConfigView, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL != "" {
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return ProxyPoolConfigView{}, ErrProxyPoolURLInvalid
		}
		if err := s.settingRepo.Set(ctx, SettingKeyProxyPoolURL, strings.TrimRight(rawURL, "/")); err != nil {
			return ProxyPoolConfigView{}, err
		}
	} else if err := s.settingRepo.Delete(ctx, SettingKeyProxyPoolURL); err != nil && !errors.Is(err, ErrSettingNotFound) {
		return ProxyPoolConfigView{}, err
	}
	if token != nil {
		if t := strings.TrimSpace(*token); t != "" {
			if err := s.settingRepo.Set(ctx, SettingKeyProxyPoolToken, t); err != nil {
				return ProxyPoolConfigView{}, err
			}
		} else if err := s.settingRepo.Delete(ctx, SettingKeyProxyPoolToken); err != nil && !errors.Is(err, ErrSettingNotFound) {
			return ProxyPoolConfigView{}, err
		}
	}
	return s.GetConfig(ctx)
}

// proxyPoolLease is the provider's lease JSON.
type proxyPoolLease struct {
	LeaseID   string    `json:"lease_id"`
	ClientRef string    `json:"client_ref"`
	Protocol  string    `json:"protocol"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	Node      string    `json:"node"`
	CreatedAt time.Time `json:"created_at"`
}

// call sends one request to the provider. It returns the HTTP status and the
// provider's error code ("pool_exhausted", …) for non-2xx answers.
func (s *ProxyPoolService) call(ctx context.Context, method, path string, body any, out any) (int, string, error) {
	baseURL, token, _, err := s.resolve(ctx)
	if err != nil {
		return 0, "", err
	}
	if baseURL == "" || token == "" {
		return 0, "", ErrProxyPoolNotConfigured
	}
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		rd = bytes.NewReader(buf)
	}
	// baseURL is the provider URL an admin configured; UpdateConfig accepts
	// http(s) only. Reaching an admin-chosen host is the point of this call.
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rd) //nolint:gosec // G704: admin-configured provider URL, see above
	if err != nil {
		return 0, "", ErrProxyPoolUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req) //nolint:gosec // G704: admin-configured provider URL, see above
	if err != nil {
		// A *url.Error repeats the URL, whose path may hold a lease ID; log
		// only the cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		log.Printf("[ProxyPool] %s %s: %v", method, proxyPoolLogPath(path), err)
		return 0, "", ErrProxyPoolUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return resp.StatusCode, e.Error, nil
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			log.Printf("[ProxyPool] %s %s: decode: %v", method, proxyPoolLogPath(path), err)
			return resp.StatusCode, "", ErrProxyPoolUnavailable
		}
	}
	return resp.StatusCode, "", nil
}

// proxyPoolLogPath hides the lease ID in a provider path: a lease ID must not
// reach the logs.
func proxyPoolLogPath(path string) string {
	const prefix = "/v1/leases/"
	if !strings.HasPrefix(path, prefix) {
		return path
	}
	rest := strings.TrimPrefix(path, prefix)
	if i := strings.Index(rest, "/"); i >= 0 {
		return prefix + "{id}" + rest[i:]
	}
	return prefix + "{id}"
}

func providerError(status int, code string) error {
	switch {
	case code == "pool_exhausted":
		return ErrProxyPoolExhausted
	case code == "no_healthy_node":
		return ErrProxyPoolNoHealthyNode
	case status == http.StatusNotFound:
		return ErrProxyPoolLeaseGone
	default:
		log.Printf("[ProxyPool] provider answered HTTP %d %q", status, code)
		return ErrProxyPoolUnavailable
	}
}

// Health returns the provider's /healthz body.
func (s *ProxyPoolService) Health(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	status, code, err := s.call(ctx, http.MethodGet, "/healthz", nil, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, providerError(status, code)
	}
	return out, nil
}

// Allocate gives an account a pool proxy: a newly leased slot when the
// provider has one, otherwise the least used shared slot that still has room
// (shared = true). The room is only a hint; saving the account enforces the
// limit under a row lock.
func (s *ProxyPoolService) Allocate(ctx context.Context) (*Proxy, bool, error) {
	proxy, err := s.leaseNew(ctx)
	if err == nil {
		return proxy, false, nil
	}
	if !errors.Is(err, ErrProxyPoolExhausted) && !errors.Is(err, ErrProxyPoolNoHealthyNode) {
		return nil, false, err
	}
	candidates, listErr := s.ListShareable(ctx)
	if listErr != nil {
		return nil, false, listErr
	}
	if len(candidates) == 0 {
		if errors.Is(err, ErrProxyPoolNoHealthyNode) {
			return nil, false, err // the slots are free; the exit nodes are not
		}
		return nil, false, ErrProxyPoolNoCapacity
	}
	picked := candidates[0].Proxy
	log.Printf("[ProxyPool] sharing proxy %d (%d accounts)", picked.ID, candidates[0].Used)
	return &picked, true, nil
}

// leaseNew leases a new proxy and stores it as a pool-managed Proxy row with
// the default share settings.
func (s *ProxyPoolService) leaseNew(ctx context.Context) (*Proxy, error) {
	shareable, shareMax, err := s.shareDefaults(ctx)
	if err != nil {
		return nil, err
	}
	var l proxyPoolLease
	status, code, err := s.call(ctx, http.MethodPost, "/v1/leases",
		map[string]string{"client_ref": s.newRef(), "protocol": "http"}, &l)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, providerError(status, code)
	}
	if l.LeaseID == "" || l.Host == "" || l.Port < 1 || l.Port > 65535 || l.Username == "" || l.Password == "" ||
		(l.Protocol != "http" && l.Protocol != "socks5") {
		log.Printf("[ProxyPool] provider returned an incomplete lease")
		s.release(ctx, l.LeaseID)
		return nil, ErrProxyPoolUnavailable
	}
	proxy := &Proxy{
		Name:           "Proxy pool · " + l.Username,
		Protocol:       l.Protocol,
		Host:           l.Host,
		Port:           l.Port,
		Username:       l.Username,
		Password:       l.Password,
		Status:         StatusActive,
		FallbackMode:   FallbackModeNone,
		ExpiryWarnDays: 7,
		ManagedBy:      ProxyManagedByPool,
		ExternalRef:    l.LeaseID,
		PoolShareable:  shareable,
		PoolShareMax:   shareMax,
	}
	if err := s.proxyRepo.Create(ctx, proxy); err != nil {
		s.release(ctx, l.LeaseID)
		return nil, err
	}
	log.Printf("[ProxyPool] leased proxy %d (%s)", proxy.ID, l.Node)
	return proxy, nil
}

// ListShareable returns the shared pool proxies that still have room, least
// used first (ties: lowest id).
func (s *ProxyPoolService) ListShareable(ctx context.Context) ([]ProxyPoolShareCandidate, error) {
	all, err := s.proxyRepo.ListAllForFallback(ctx)
	if err != nil {
		return nil, err
	}
	occupancy, err := s.proxyRepo.GetPoolOccupancy(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProxyPoolShareCandidate, 0)
	for i := range all {
		p := all[i]
		if p.ManagedBy != ProxyManagedByPool || !p.PoolShareable {
			continue
		}
		used := occupancy[p.ID]
		if capacity, limited := p.PoolCapacity(); limited && used >= capacity {
			continue
		}
		out = append(out, ProxyPoolShareCandidate{Proxy: p, Used: used})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Used != out[j].Used {
			return out[i].Used < out[j].Used
		}
		return out[i].Proxy.ID < out[j].Proxy.ID
	})
	return out, nil
}

// Rotate moves a pool-managed proxy to another exit node and returns the new
// node's name. The proxy row does not change.
func (s *ProxyPoolService) Rotate(ctx context.Context, proxyID int64) (string, error) {
	proxy, err := s.proxyRepo.GetByID(ctx, proxyID)
	if err != nil {
		return "", err
	}
	if proxy.ManagedBy != ProxyManagedByPool || proxy.ExternalRef == "" {
		return "", ErrProxyNotPoolManaged
	}
	var l proxyPoolLease
	status, code, err := s.call(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(proxy.ExternalRef)+"/rotate", nil, &l)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", providerError(status, code)
	}
	log.Printf("[ProxyPool] rotated proxy %d to %s", proxy.ID, l.Node)
	return l.Node, nil
}

// release deletes a lease at the provider. true means it is gone (deleted
// now or unknown already).
func (s *ProxyPoolService) release(ctx context.Context, leaseID string) bool {
	if leaseID == "" {
		return true
	}
	status, _, err := s.call(ctx, http.MethodDelete, "/v1/leases/"+url.PathEscape(leaseID), nil, nil)
	if err != nil {
		return false
	}
	return status == http.StatusNoContent || status == http.StatusNotFound
}

// Reconcile releases (1) pool-managed rows that are at least
// proxyPoolRowGrace old and used by no account and no proxy as its backup,
// and (2) provider leases made
// by Sub2API that no pool-managed row refers to (a crash between lease and
// row, or a row an admin deleted by hand). It returns how many rows and how
// many orphan leases it released.
func (s *ProxyPoolService) Reconcile(ctx context.Context) (rows int, orphans int, err error) {
	baseURL, token, _, err := s.resolve(ctx)
	if err != nil {
		return 0, 0, err
	}
	if baseURL == "" || token == "" {
		return 0, 0, nil
	}
	all, err := s.proxyRepo.ListAllForFallback(ctx)
	if err != nil {
		return 0, 0, err
	}
	now := s.now()
	// A row another proxy falls back to is in use too: deleting it would
	// clear that backup_proxy_id (ON DELETE SET NULL) without a word.
	backups := map[int64]bool{}
	for i := range all {
		if all[i].BackupProxyID != nil {
			backups[*all[i].BackupProxyID] = true
		}
	}
	refs := map[string]bool{}
	for i := range all {
		p := &all[i]
		if p.ManagedBy != ProxyManagedByPool {
			continue
		}
		if now.Sub(p.CreatedAt) < proxyPoolRowGrace || backups[p.ID] {
			refs[p.ExternalRef] = true
			continue
		}
		count, err := s.proxyRepo.CountAccountsByProxyID(ctx, p.ID)
		if err != nil {
			return rows, orphans, err
		}
		if count > 0 {
			refs[p.ExternalRef] = true
			continue
		}
		if !s.release(ctx, p.ExternalRef) {
			refs[p.ExternalRef] = true // try again next round
			continue
		}
		refs[p.ExternalRef] = true // released just now; not an orphan below
		if err := s.proxyRepo.Delete(ctx, p.ID); err != nil {
			return rows, orphans, err
		}
		rows++
		log.Printf("[ProxyPool] released unused proxy %d", p.ID)
	}

	var list struct {
		Leases []proxyPoolLease `json:"leases"`
	}
	status, code, err := s.call(ctx, http.MethodGet, "/v1/leases", nil, &list)
	if err != nil {
		return rows, orphans, err
	}
	if status != http.StatusOK {
		return rows, orphans, providerError(status, code)
	}
	for _, l := range list.Leases {
		if !strings.HasPrefix(l.ClientRef, proxyPoolClientRefPrefix) || refs[l.LeaseID] {
			continue
		}
		if now.Sub(l.CreatedAt) < proxyPoolOrphanLeaseGrace {
			continue
		}
		if s.release(ctx, l.LeaseID) {
			orphans++
			log.Printf("[ProxyPool] released an orphan lease")
		}
	}
	return rows, orphans, nil
}

// Start runs Reconcile every interval until Stop.
func (s *ProxyPoolService) Start() {
	if s == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				if _, _, err := s.Reconcile(ctx); err != nil {
					log.Printf("[ProxyPool] reconcile: %v", err)
				}
				cancel()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *ProxyPoolService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}
