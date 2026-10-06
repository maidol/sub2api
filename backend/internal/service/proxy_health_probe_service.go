package service

import (
	"context"
	"log"
	"sync"
	"time"
)

const (
	proxyHealthProbeRequestTimeout = 10 * time.Second
	proxyHealthProbeBudgetMargin   = 5 * time.Second
	proxyHealthProbeConcurrency    = 4
	proxyHealthProbeDefaultURLs    = 2
)

func proxyHealthProbeTimeout(urlCount int) time.Duration {
	if urlCount <= 0 {
		urlCount = proxyHealthProbeDefaultURLs
	}
	return time.Duration(urlCount)*proxyHealthProbeRequestTimeout + proxyHealthProbeBudgetMargin
}

type ProxyHealthProbeService struct {
	proxyRepo    ProxyRepository
	adminService AdminService
	interval     time.Duration
	timeout      time.Duration
	stopCh       chan struct{}
	stopOnce     sync.Once
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

func NewProxyHealthProbeService(proxyRepo ProxyRepository, adminService AdminService, interval, timeout time.Duration) *ProxyHealthProbeService {
	if timeout <= 0 {
		timeout = proxyHealthProbeTimeout(0)
	}
	return &ProxyHealthProbeService{proxyRepo: proxyRepo, adminService: adminService, interval: interval, timeout: timeout, stopCh: make(chan struct{})}
}

func (s *ProxyHealthProbeService) Start() {
	if s == nil || s.proxyRepo == nil || s.adminService == nil || s.interval <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Go(func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		s.runOnce(ctx)
		for {
			select {
			case <-ticker.C:
				s.runOnce(ctx)
			case <-s.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	})
}

func (s *ProxyHealthProbeService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
		if s.cancel != nil {
			s.cancel()
		}
	})
	s.wg.Wait()
}

func (s *ProxyHealthProbeService) runOnce(ctx context.Context) {
	proxies, err := s.proxyRepo.ListActiveWithAccountCount(ctx)
	if err != nil {
		log.Printf("[ProxyHealthProbe] list active proxies failed: %v", err)
		return
	}

	now := time.Now()
	candidates := make([]ProxyWithAccountCount, 0, len(proxies))
	for i := range proxies {
		proxy := proxies[i]
		if proxy.Status != StatusActive || proxy.AccountCount <= 0 || proxy.IsExpired(now) {
			continue
		}
		candidates = append(candidates, proxy)
	}
	if len(candidates) == 0 {
		log.Printf("[ProxyHealthProbe] checked=0 unreachable=0")
		return
	}

	jobs := make(chan ProxyWithAccountCount, len(candidates))
	var workers sync.WaitGroup
	var statsMu sync.Mutex
	attempted, failed := 0, 0
	for range proxyHealthProbeConcurrency {
		workers.Go(func() {
			for candidate := range jobs {
				probeCtx, cancel := context.WithTimeout(ctx, s.timeout)
				err := s.adminService.ProbeProxyLatency(probeCtx, &candidate.Proxy)
				cancel()
				statsMu.Lock()
				attempted++
				if err != nil {
					failed++
				}
				statsMu.Unlock()
			}
		})
	}
	for i := range candidates {
		jobs <- candidates[i]
	}
	close(jobs)
	workers.Wait()
	// 汇总里不要出现 " failed" / "error" / "warn" 等词：stdlog 桥按关键词推断级别，
	// 会把每一轮正常汇总记成 ERROR 并写进运维系统日志。
	log.Printf("[ProxyHealthProbe] checked=%d unreachable=%d", attempted, failed)
}
