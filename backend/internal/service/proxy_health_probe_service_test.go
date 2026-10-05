package service

import (
	"bytes"
	"context"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type proxyHealthProbeRepositoryStub struct {
	ProxyRepository
	proxies []ProxyWithAccountCount
	err     error
	calls   atomic.Int64
}

func (r *proxyHealthProbeRepositoryStub) ListActiveWithAccountCount(context.Context) ([]ProxyWithAccountCount, error) {
	r.calls.Add(1)
	return r.proxies, r.err
}

type proxyHealthProbeAdminStub struct {
	AdminService
	mu             sync.Mutex
	ids            []int64
	active         int
	maxActive      int
	entered        chan int64
	completed      chan int64
	release        chan struct{}
	blockUntilDone map[int64]bool
	errors         map[int64]error
}

func (s *proxyHealthProbeAdminStub) ProbeProxyLatency(ctx context.Context, proxy *Proxy) error {
	s.mu.Lock()
	s.ids = append(s.ids, proxy.ID)
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()

	if s.entered != nil {
		s.entered <- proxy.ID
	}
	if s.release != nil && s.blockUntilDone[proxy.ID] {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.completed != nil {
		s.completed <- proxy.ID
	}
	return s.errors[proxy.ID]
}

func (s *proxyHealthProbeAdminStub) snapshot() (ids []int64, maxActive int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ids), s.maxActive
}

type proxyHealthProbeLatencyCacheStub struct {
	existing map[int64]*ProxyLatencyInfo
	written  map[int64]*ProxyLatencyInfo
	getErr   error
	setErr   error
}

func (c *proxyHealthProbeLatencyCacheStub) GetProxyLatencies(_ context.Context, ids []int64) (map[int64]*ProxyLatencyInfo, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	result := make(map[int64]*ProxyLatencyInfo, len(ids))
	for _, id := range ids {
		if info := c.existing[id]; info != nil {
			result[id] = info
		}
	}
	return result, nil
}

func (c *proxyHealthProbeLatencyCacheStub) SetProxyLatency(_ context.Context, id int64, info *ProxyLatencyInfo) error {
	if c.setErr != nil {
		return c.setErr
	}
	if c.written == nil {
		c.written = make(map[int64]*ProxyLatencyInfo)
	}
	copy := *info
	c.written[id] = &copy
	return nil
}

type proxyHealthProbeProberStub struct {
	info    *ProxyExitInfo
	latency int64
	err     error
	calls   int
}

func (p *proxyHealthProbeProberStub) ProbeProxy(context.Context, string) (*ProxyExitInfo, int64, error) {
	p.calls++
	return p.info, p.latency, p.err
}

type proxyHealthProbeFallbackProber struct {
	secondURLSucceeded atomic.Bool
}

func (p *proxyHealthProbeFallbackProber) ProbeProxy(ctx context.Context, _ string) (*ProxyExitInfo, int64, error) {
	firstRequestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	<-firstRequestCtx.Done()
	cancel()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	p.secondURLSucceeded.Store(true)
	return &ProxyExitInfo{IP: "203.0.113.10", CountryCode: "US"}, 42, nil
}

func TestProvideProxyHealthProbeServiceUsesConfigIntervalAndURLCount(t *testing.T) {
	cfg := &config.Config{
		ProxyHealthProbeIntervalMinutes: 0,
		Security: config.SecurityConfig{ProxyProbe: config.ProxyProbeConfig{URLs: []config.ProbeURLConfig{
			{URL: "https://one.example", Parser: "ipify"},
			{URL: "https://two.example", Parser: "ipify"},
			{URL: "https://three.example", Parser: "ipify"},
		}}},
	}

	svc := ProvideProxyHealthProbeService(cfg, nil, nil)
	if svc.interval != 0 {
		t.Fatalf("interval = %s, want disabled interval 0", svc.interval)
	}
	if svc.timeout != 35*time.Second {
		t.Fatalf("timeout = %s, want 35s for three configured URLs", svc.timeout)
	}
}

func TestProvideProxyHealthProbeServiceUsesIntervalAndConfiguredURLCount(t *testing.T) {
	cfg := &config.Config{
		ProxyHealthProbeIntervalMinutes: 0,
		Security: config.SecurityConfig{ProxyProbe: config.ProxyProbeConfig{URLs: []config.ProbeURLConfig{
			{URL: "https://one.example", Parser: "ipify"},
			{URL: "https://two.example", Parser: "ipify"},
			{URL: "https://three.example", Parser: "ipify"},
		}}},
	}

	svc := ProvideProxyHealthProbeService(cfg, nil, nil)
	if svc.interval != 0 {
		t.Fatalf("interval = %s, want disabled interval", svc.interval)
	}
	if svc.timeout != 35*time.Second {
		t.Fatalf("timeout = %s, want 35s for three URLs", svc.timeout)
	}
}

func TestProxyHealthProbeTimeoutUsesURLCountAndMargin(t *testing.T) {
	for _, test := range []struct {
		name     string
		urlCount int
		want     time.Duration
	}{
		{name: "default built-ins", urlCount: 0, want: 25 * time.Second},
		{name: "negative uses default", urlCount: -1, want: 25 * time.Second},
		{name: "one custom URL", urlCount: 1, want: 15 * time.Second},
		{name: "two URLs", urlCount: 2, want: 25 * time.Second},
		{name: "three custom URLs", urlCount: 3, want: 35 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := proxyHealthProbeTimeout(test.urlCount); got != test.want {
				t.Errorf("proxyHealthProbeTimeout(%d) = %s, want %s", test.urlCount, got, test.want)
			}
		})
	}
}

func TestProxyHealthProbeRunOnceSelectsActiveBoundUnexpiredProxies(t *testing.T) {
	expired := time.Now().Add(-time.Second)
	repo := &proxyHealthProbeRepositoryStub{proxies: []ProxyWithAccountCount{
		{Proxy: Proxy{ID: 1, Status: StatusActive}, AccountCount: 2},
		{Proxy: Proxy{ID: 2, Status: StatusDisabled}, AccountCount: 1},
		{Proxy: Proxy{ID: 3, Status: StatusActive}, AccountCount: 0},
		{Proxy: Proxy{ID: 4, Status: StatusActive, ExpiresAt: &expired}, AccountCount: 1},
		{Proxy: Proxy{ID: 5, Status: StatusActive, ManagedBy: ProxyManagedByPool}, AccountCount: 1},
	}}
	admin := &proxyHealthProbeAdminStub{}
	svc := NewProxyHealthProbeService(repo, admin, time.Minute, time.Second)

	svc.runOnce(t.Context())

	ids, _ := admin.snapshot()
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{1, 5}) {
		t.Fatalf("probed IDs = %v, want [1 5]", ids)
	}
}

func TestProxyHealthProbeRunOnceContinuesAfterProbeFailure(t *testing.T) {
	probeErr := errors.New("connection refused")
	repo := &proxyHealthProbeRepositoryStub{proxies: []ProxyWithAccountCount{
		{Proxy: Proxy{ID: 1, Status: StatusActive}, AccountCount: 1},
		{Proxy: Proxy{ID: 2, Status: StatusActive}, AccountCount: 1},
	}}
	admin := &proxyHealthProbeAdminStub{errors: map[int64]error{1: probeErr}}
	svc := NewProxyHealthProbeService(repo, admin, time.Minute, time.Second)

	svc.runOnce(t.Context())

	ids, _ := admin.snapshot()
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{1, 2}) {
		t.Fatalf("probed IDs = %v, want [1 2]", ids)
	}
}

func TestProxyHealthProbeRunOnceLimitsConcurrentProbesToFour(t *testing.T) {
	const proxyCount = 9
	repo := &proxyHealthProbeRepositoryStub{}
	for id := range proxyCount {
		repo.proxies = append(repo.proxies, ProxyWithAccountCount{
			Proxy: Proxy{ID: int64(id + 1), Status: StatusActive}, AccountCount: 1,
		})
	}
	release := make(chan struct{})
	blockUntilDone := make(map[int64]bool, proxyCount)
	for id := range proxyCount {
		blockUntilDone[int64(id+1)] = true
	}
	admin := &proxyHealthProbeAdminStub{
		entered:        make(chan int64, proxyCount),
		release:        release,
		blockUntilDone: blockUntilDone,
	}
	svc := NewProxyHealthProbeService(repo, admin, time.Minute, time.Second)
	done := make(chan struct{})
	go func() {
		svc.runOnce(t.Context())
		close(done)
	}()

	for range 4 {
		select {
		case <-admin.entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("timed out waiting for the first four probes")
		}
	}
	select {
	case id := <-admin.entered:
		close(release)
		t.Fatalf("probe %d started while four probes were blocked", id)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for probes to finish")
	}
	ids, maxActive := admin.snapshot()
	if len(ids) != proxyCount {
		t.Fatalf("probed %d proxies, want %d", len(ids), proxyCount)
	}
	if maxActive > 4 {
		t.Fatalf("maximum active probes = %d, want <= 4", maxActive)
	}
}

func TestProxyHealthProbeTimeoutForOneProxyDoesNotCancelAnother(t *testing.T) {
	repo := &proxyHealthProbeRepositoryStub{proxies: []ProxyWithAccountCount{
		{Proxy: Proxy{ID: 1, Status: StatusActive}, AccountCount: 1},
		{Proxy: Proxy{ID: 2, Status: StatusActive}, AccountCount: 1},
	}}
	admin := &proxyHealthProbeAdminStub{
		entered:        make(chan int64, 2),
		completed:      make(chan int64, 2),
		release:        make(chan struct{}),
		blockUntilDone: map[int64]bool{1: true},
	}
	svc := NewProxyHealthProbeService(repo, admin, time.Minute, 250*time.Millisecond)
	runDone := make(chan struct{})
	go func() {
		svc.runOnce(t.Context())
		close(runDone)
	}()

	deadline := time.After(150 * time.Millisecond)
	completedID := int64(0)
	for completedID != 2 {
		select {
		case completedID = <-admin.completed:
		case <-deadline:
			t.Fatal("second proxy did not succeed independently while first probe was blocked")
		}
	}
	select {
	case <-runDone:
		t.Fatal("run ended when the independent probe completed, before the first probe timed out")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("run did not finish after the blocked proxy's deadline")
	}
	ids, _ := admin.snapshot()
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{1, 2}) {
		t.Fatalf("probed IDs = %v, want [1 2]", ids)
	}
}

func TestAdminServiceProbeProxyLatencyPersistsConnectivityAndQuality(t *testing.T) {
	proxy := &Proxy{ID: 12, Protocol: "http", Host: "proxy.example", Port: 8080}
	qualityScore := 97
	qualityCheckedAt := int64(1791200000)
	cache := &proxyHealthProbeLatencyCacheStub{existing: map[int64]*ProxyLatencyInfo{
		proxy.ID: {
			QualityStatus:    "healthy",
			QualityScore:     &qualityScore,
			QualitySummary:   "all checks passed",
			QualityCheckedAt: &qualityCheckedAt,
		},
	}}
	prober := &proxyHealthProbeProberStub{
		info:    &ProxyExitInfo{IP: "203.0.113.10", CountryCode: "US"},
		latency: 42,
	}
	admin := &adminServiceImpl{proxyProber: prober, proxyLatencyCache: cache}

	if err := admin.ProbeProxyLatency(t.Context(), proxy); err != nil {
		t.Fatalf("ProbeProxyLatency() error = %v, want nil", err)
	}
	got := cache.written[proxy.ID]
	if got == nil || !got.Success || got.LatencyMs == nil || *got.LatencyMs != 42 || got.IPAddress != "203.0.113.10" {
		t.Fatalf("persisted connectivity = %+v, want successful probe result", got)
	}
	if got.QualityStatus != "healthy" || got.QualitySummary != "all checks passed" || got.QualityScore == nil || *got.QualityScore != qualityScore || got.QualityCheckedAt == nil || *got.QualityCheckedAt != qualityCheckedAt {
		t.Fatalf("quality snapshot changed after connectivity probe: %+v", got)
	}
}

func TestAdminServiceProbeProxyLatencyNoOpsWithoutProberOrProxy(t *testing.T) {
	cache := &proxyHealthProbeLatencyCacheStub{}
	admin := &adminServiceImpl{proxyLatencyCache: cache}
	proxy := &Proxy{ID: 15}
	if err := admin.ProbeProxyLatency(t.Context(), proxy); err != nil {
		t.Fatalf("ProbeProxyLatency() without prober error = %v, want nil", err)
	}
	if err := (&adminServiceImpl{proxyProber: &proxyHealthProbeProberStub{}, proxyLatencyCache: cache}).ProbeProxyLatency(t.Context(), nil); err != nil {
		t.Fatalf("ProbeProxyLatency() with nil proxy error = %v, want nil", err)
	}
	if len(cache.written) != 0 {
		t.Errorf("cache writes = %v, want no writes", cache.written)
	}
}

func TestAdminServiceProbeProxyLatencyPersistsFailure(t *testing.T) {
	proxy := &Proxy{ID: 13, Protocol: "http", Host: "proxy.example", Port: 8080}
	probeErr := errors.New("proxy connection failed")
	cache := &proxyHealthProbeLatencyCacheStub{}
	admin := &adminServiceImpl{
		proxyProber:       &proxyHealthProbeProberStub{err: probeErr},
		proxyLatencyCache: cache,
	}

	err := admin.ProbeProxyLatency(t.Context(), proxy)
	if !errors.Is(err, probeErr) {
		t.Fatalf("ProbeProxyLatency() error = %v, want %v", err, probeErr)
	}
	got := cache.written[proxy.ID]
	if got == nil || got.Success || got.Message != probeErr.Error() || got.UpdatedAt.IsZero() {
		t.Fatalf("persisted failure = %+v, want failed result with message and timestamp", got)
	}
}

func TestProxyHealthProbeFallbackUsesWorkerBudgetAndRealSavePath(t *testing.T) {
	proxy := &Proxy{ID: 14, Status: StatusActive, Protocol: "http", Host: "proxy.example", Port: 8080}
	cache := &proxyHealthProbeLatencyCacheStub{}
	prober := &proxyHealthProbeFallbackProber{}
	admin := &adminServiceImpl{proxyProber: prober, proxyLatencyCache: cache}
	repo := &proxyHealthProbeRepositoryStub{proxies: []ProxyWithAccountCount{{Proxy: *proxy, AccountCount: 1}}}
	svc := NewProxyHealthProbeService(repo, admin, time.Minute, proxyHealthProbeTimeout(2))

	svc.runOnce(t.Context())

	if !prober.secondURLSucceeded.Load() {
		t.Fatal("fallback URL did not get a chance to run after the first URL timed out")
	}
	got := cache.written[proxy.ID]
	if got == nil || !got.Success || got.LatencyMs == nil || *got.LatencyMs != 42 || got.IPAddress != "203.0.113.10" {
		t.Fatalf("persisted fallback result = %+v, want successful URL-2 result", got)
	}
}

func TestProxyHealthProbeStartWithNonPositiveIntervalDoesNotRun(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		repo := &proxyHealthProbeRepositoryStub{}
		svc := NewProxyHealthProbeService(repo, &proxyHealthProbeAdminStub{}, interval, time.Second)
		svc.Start()
		svc.Stop()
		if got := repo.calls.Load(); got != 0 {
			t.Errorf("interval %s started %d rounds, want zero", interval, got)
		}
	}
}

func TestProxyHealthProbeStartRunsImmediatelyAndStopDrains(t *testing.T) {
	repo := &proxyHealthProbeRepositoryStub{
		proxies: []ProxyWithAccountCount{{Proxy: Proxy{ID: 1, Status: StatusActive}, AccountCount: 1}},
	}
	admin := &proxyHealthProbeAdminStub{
		entered:        make(chan int64, 1),
		release:        make(chan struct{}),
		blockUntilDone: map[int64]bool{1: true},
	}
	svc := NewProxyHealthProbeService(repo, admin, time.Hour, time.Hour)

	svc.Start()
	select {
	case <-admin.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("initial probe did not start immediately")
	}
	stopped := make(chan struct{})
	go func() {
		svc.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not drain active probes promptly")
	}
}

func TestProxyHealthProbeRunOnceLogsOneAggregateLine(t *testing.T) {
	var output bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(originalWriter) })

	repo := &proxyHealthProbeRepositoryStub{proxies: []ProxyWithAccountCount{
		{Proxy: Proxy{ID: 1, Status: StatusActive}, AccountCount: 1},
		{Proxy: Proxy{ID: 2, Status: StatusActive}, AccountCount: 1},
	}}
	admin := &proxyHealthProbeAdminStub{errors: map[int64]error{
		1: errors.New("first failure"),
		2: errors.New("second failure"),
	}}
	svc := NewProxyHealthProbeService(repo, admin, time.Minute, time.Second)

	svc.runOnce(t.Context())

	if got := strings.Count(strings.TrimSpace(output.String()), "\n"); got != 0 {
		t.Fatalf("round logged %d lines, want exactly one: %q", got+1, output.String())
	}
	if !strings.Contains(output.String(), "checked=2") || !strings.Contains(output.String(), "failed=2") {
		t.Fatalf("round summary = %q, want checked=2 and failed=2", output.String())
	}
}

func TestProxyHealthProbeRunOnceLogMentionsListError(t *testing.T) {
	var output bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(originalWriter) })
	repoErr := errors.New("database unavailable")
	repo := &proxyHealthProbeRepositoryStub{err: repoErr}
	svc := NewProxyHealthProbeService(repo, &proxyHealthProbeAdminStub{}, time.Minute, time.Second)

	svc.runOnce(t.Context())

	if !strings.Contains(output.String(), repoErr.Error()) {
		t.Fatalf("round summary = %q, want repository error", output.String())
	}
	if got := strings.Count(strings.TrimSpace(output.String()), "\n"); got != 0 {
		t.Fatalf("round logged %d lines, want exactly one: %q", got+1, output.String())
	}
}
