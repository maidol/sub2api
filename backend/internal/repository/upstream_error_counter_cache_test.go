package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newUpstreamErrorCounterTestCache(t *testing.T) (*miniredis.Miniredis, *upstreamErrorCounterCache) {
	t.Helper()
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return mini, &upstreamErrorCounterCache{rdb: client}
}

func TestUpstreamErrorCounterCacheRollingFiveMinuteBoundaryAndBuckets(t *testing.T) {
	mini, cache := newUpstreamErrorCounterTestCache(t)
	base := time.Unix(1_700_000_000, 0)
	mini.SetTime(base)

	// Both old events are outside the rolling window after the clock advances.
	require.NoError(t, cache.RecordUpstreamError(context.Background(), service.OpsUpstreamErrorEvent{AccountID: 7, UpstreamStatusCode: 400}))
	require.NoError(t, cache.RecordUpstreamError(context.Background(), service.OpsUpstreamErrorEvent{AccountID: 7, UpstreamStatusCode: 503}))
	mini.SetTime(base.Add(5*time.Minute + time.Second))
	require.NoError(t, cache.RecordUpstreamError(context.Background(), service.OpsUpstreamErrorEvent{AccountID: 7, UpstreamStatusCode: 429}))

	counts, err := cache.GetAccountUpstreamErrorCounts(context.Background(), []int64{7})
	require.NoError(t, err)
	require.Equal(t, map[int64]service.UpstreamErrorCounts{7: {Client: 1}}, counts)

	// A server error is counted independently in the 5xx bucket.
	require.NoError(t, cache.RecordUpstreamError(context.Background(), service.OpsUpstreamErrorEvent{AccountID: 7, UpstreamStatusCode: 500}))
	counts, err = cache.GetAccountUpstreamErrorCounts(context.Background(), []int64{7})
	require.NoError(t, err)
	require.Equal(t, service.UpstreamErrorCounts{Client: 1, Server: 1}, counts[7])
}

func TestUpstreamErrorCounterCacheSafeNoopForNilInvalidAndEmptyInputs(t *testing.T) {
	var nilCache *upstreamErrorCounterCache
	require.NoError(t, nilCache.RecordUpstreamError(context.Background(), service.OpsUpstreamErrorEvent{AccountID: 1, UpstreamStatusCode: 500}))
	counts, err := nilCache.GetAccountUpstreamErrorCounts(context.Background(), []int64{1})
	require.NoError(t, err)
	require.Empty(t, counts)

	_, cache := newUpstreamErrorCounterTestCache(t)
	for _, event := range []service.OpsUpstreamErrorEvent{
		{AccountID: 0, UpstreamStatusCode: 500},
		{AccountID: 1, UpstreamStatusCode: 200},
	} {
		require.NoError(t, cache.RecordUpstreamError(context.Background(), event))
	}
	counts, err = cache.GetAccountUpstreamErrorCounts(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, counts)
}

func TestUpstreamErrorCounterCacheRedisFailureIsReported(t *testing.T) {
	mini, cache := newUpstreamErrorCounterTestCache(t)
	mini.Close()

	err := cache.RecordUpstreamError(context.Background(), service.OpsUpstreamErrorEvent{AccountID: 1, UpstreamStatusCode: 500})
	require.Error(t, err)
	_, err = cache.GetAccountUpstreamErrorCounts(context.Background(), []int64{1})
	require.Error(t, err)
}
