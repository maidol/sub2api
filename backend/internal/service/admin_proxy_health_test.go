package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type proxyLatencyCacheStub struct {
	result map[int64]*ProxyLatencyInfo
	err    error
	calls  int
	ids    []int64
	ctx    context.Context
}

func (s *proxyLatencyCacheStub) GetProxyLatencies(ctx context.Context, ids []int64) (map[int64]*ProxyLatencyInfo, error) {
	s.calls++
	s.ctx = ctx
	s.ids = append([]int64(nil), ids...)
	return s.result, s.err
}

func (s *proxyLatencyCacheStub) SetProxyLatency(context.Context, int64, *ProxyLatencyInfo) error {
	return nil
}

func TestAdminServiceGetProxyLatenciesReturnsCacheResult(t *testing.T) {
	info := &ProxyLatencyInfo{Success: true}
	result := map[int64]*ProxyLatencyInfo{12: info}
	cache := &proxyLatencyCacheStub{result: result}
	ids := []int64{12, 34, 56}
	ctx := t.Context()

	got, err := AdminService(&adminServiceImpl{proxyLatencyCache: cache}).GetProxyLatencies(ctx, ids)
	if err != nil {
		t.Fatalf("GetProxyLatencies() error = %v, want nil", err)
	}
	if got[12] != info {
		t.Errorf("GetProxyLatencies() result = %#v, want cache result %#v", got, result)
	}
	got[78] = &ProxyLatencyInfo{}
	if _, ok := result[78]; !ok {
		t.Error("GetProxyLatencies() returned a copy instead of the cache map")
	}
	if cache.calls != 1 {
		t.Errorf("cache calls = %d, want 1", cache.calls)
	}
	if !reflect.DeepEqual(cache.ids, ids) {
		t.Errorf("cache IDs = %v, want exact IDs %v", cache.ids, ids)
	}
	if cache.ctx != ctx {
		t.Error("cache received a different context")
	}
}

func TestAdminServiceGetProxyLatenciesNilCacheReturnsEmptyMap(t *testing.T) {
	got, err := AdminService(&adminServiceImpl{}).GetProxyLatencies(t.Context(), []int64{1})
	if err != nil {
		t.Fatalf("GetProxyLatencies() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("GetProxyLatencies() returned a nil map, want a non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("GetProxyLatencies() returned %v, want an empty map", got)
	}
}

func TestAdminServiceGetProxyLatenciesEmptyIDsDoesNotCallCache(t *testing.T) {
	cache := &proxyLatencyCacheStub{}

	got, err := AdminService(&adminServiceImpl{proxyLatencyCache: cache}).GetProxyLatencies(t.Context(), nil)
	if err != nil {
		t.Fatalf("GetProxyLatencies() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("GetProxyLatencies() returned a nil map, want a non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("GetProxyLatencies() returned %v, want an empty map", got)
	}
	if cache.calls != 0 {
		t.Errorf("cache calls = %d, want 0", cache.calls)
	}
}

func TestAdminServiceGetProxyLatenciesReturnsCacheErrorUnchanged(t *testing.T) {
	cacheErr := errors.New("cache unavailable")
	cache := &proxyLatencyCacheStub{err: cacheErr}

	got, err := AdminService(&adminServiceImpl{proxyLatencyCache: cache}).GetProxyLatencies(t.Context(), []int64{7})
	if err != cacheErr {
		t.Errorf("GetProxyLatencies() error = %v, want unchanged error %v", err, cacheErr)
	}
	if got != nil {
		t.Errorf("GetProxyLatencies() map = %v, want nil when cache errors", got)
	}
	if cache.calls != 1 {
		t.Errorf("cache calls = %d, want 1", cache.calls)
	}
}
