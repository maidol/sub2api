package repository

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	upstreamErrorCounterKeyPrefix = "ops:upstream_error:account:"
	upstreamErrorCounterWindow    = 5 * time.Minute
	upstreamErrorCounterTTL       = 6 * time.Minute
	upstreamErrorCounterTimeout   = 250 * time.Millisecond
)

type upstreamErrorCounterCache struct {
	rdb *redis.Client
}

func NewUpstreamErrorCounterCache(rdb *redis.Client) service.UpstreamErrorCounter {
	return &upstreamErrorCounterCache{rdb: rdb}
}

func (c *upstreamErrorCounterCache) RecordUpstreamError(ctx context.Context, event service.OpsUpstreamErrorEvent) error {
	key, ok := upstreamErrorCounterKey(event.AccountID, event.UpstreamStatusCode)
	if !ok || c == nil || c.rdb == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamErrorCounterTimeout)
	defer cancel()

	now, err := c.rdb.Time(ctx).Result()
	if err != nil {
		return fmt.Errorf("get upstream error counter time: %w", err)
	}
	nowMillis := now.UnixMilli()
	cutoff := nowMillis - upstreamErrorCounterWindow.Milliseconds()
	// UUID members avoid collisions between application processes and requests
	// sharing the same Redis sorted set score.
	member := uuid.NewString()
	pipe := c.rdb.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(nowMillis), Member: member})
	pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(cutoff, 10))
	pipe.Expire(ctx, key, upstreamErrorCounterTTL)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return fmt.Errorf("record upstream error: %w", err)
	}
	return nil
}

func (c *upstreamErrorCounterCache) GetAccountUpstreamErrorCounts(ctx context.Context, accountIDs []int64) (map[int64]service.UpstreamErrorCounts, error) {
	counts := make(map[int64]service.UpstreamErrorCounts)
	if len(accountIDs) == 0 || c == nil || c.rdb == nil {
		return counts, nil
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamErrorCounterTimeout)
	defer cancel()

	now, err := c.rdb.Time(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("get upstream error counter time: %w", err)
	}
	cutoff := now.UnixMilli() - upstreamErrorCounterWindow.Milliseconds()
	pipe := c.rdb.Pipeline()
	type accountCommands struct {
		accountID int64
		client    *redis.IntCmd
		server    *redis.IntCmd
	}
	commands := make([]accountCommands, 0, len(accountIDs))
	minScore := strconv.FormatInt(cutoff+1, 10)
	for _, accountID := range accountIDs {
		clientKey, _ := upstreamErrorCounterKey(accountID, 400)
		serverKey, _ := upstreamErrorCounterKey(accountID, 500)
		pipe.ZRemRangeByScore(ctx, clientKey, "-inf", strconv.FormatInt(cutoff, 10))
		pipe.ZRemRangeByScore(ctx, serverKey, "-inf", strconv.FormatInt(cutoff, 10))
		commands = append(commands, accountCommands{
			accountID: accountID,
			client:    pipe.ZCount(ctx, clientKey, minScore, "+inf"),
			server:    pipe.ZCount(ctx, serverKey, minScore, "+inf"),
		})
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("read upstream error counts: %w", err)
	}
	for _, command := range commands {
		count := service.UpstreamErrorCounts{Client: int(command.client.Val()), Server: int(command.server.Val())}
		if count.Client != 0 || count.Server != 0 {
			counts[command.accountID] = count
		}
	}
	return counts, nil
}

func upstreamErrorCounterKey(accountID int64, statusCode int) (string, bool) {
	if accountID <= 0 {
		return "", false
	}
	var bucket string
	switch statusCode / 100 {
	case 4:
		bucket = "4xx"
	case 5:
		bucket = "5xx"
	default:
		return "", false
	}
	return upstreamErrorCounterKeyPrefix + strconv.FormatInt(accountID, 10) + ":" + bucket, true
}
