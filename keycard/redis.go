package keycard

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keycardHashKey = "keycard"
	keycardExpiry  = 10 * time.Second
)

type RedisClient struct {
	client *redis.Client
	logger *slog.Logger
	ctx    context.Context
}

func NewRedisClient(addr string, logger *slog.Logger) *RedisClient {
	client := redis.NewClient(&redis.Options{
		Addr: addr,
	})
	return &RedisClient{
		client: client,
		logger: logger,
		ctx:    context.Background(),
	}
}

func (r *RedisClient) Close() error {
	return r.client.Close()
}

func (r *RedisClient) PublishAuth(uid string) error {
	pipe := r.client.Pipeline()

	pipe.HSet(r.ctx, keycardHashKey, "authentication", "passed")
	pipe.HSet(r.ctx, keycardHashKey, "type", "scooter")
	pipe.HSet(r.ctx, keycardHashKey, "uid", uid)
	pipe.Publish(r.ctx, keycardHashKey, "authentication")
	pipe.Expire(r.ctx, keycardHashKey, keycardExpiry)

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Error("Failed to publish auth", "error", err)
		return fmt.Errorf("failed to publish auth: %w", err)
	}

	r.logger.Info("Published authentication", "uid", uid)
	return nil
}
