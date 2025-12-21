package keycard

import (
	"fmt"
	"log/slog"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

const (
	keycardHashKey = "keycard"
	keycardExpiry  = 10 * time.Second
)

type RedisClient struct {
	client *ipc.Client
	logger *slog.Logger
}

func NewRedisClient(addr string, logger *slog.Logger) (*RedisClient, error) {
	client, err := ipc.New(
		ipc.WithURL(addr),
		ipc.WithLogger(logger),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &RedisClient{
		client: client,
		logger: logger,
	}, nil
}

func (r *RedisClient) Close() error {
	return r.client.Close()
}

func (r *RedisClient) PublishAuth(uid string) error {
	err := r.client.Hash(keycardHashKey).SetManyPublishOne(map[string]any{
		"authentication": "passed",
		"type":           "scooter",
		"uid":            uid,
	}, "authentication")
	if err != nil {
		r.logger.Error("Failed to publish auth", "error", err)
		return fmt.Errorf("failed to publish auth: %w", err)
	}

	r.client.Expire(keycardHashKey, keycardExpiry)

	r.logger.Info("Published authentication", "uid", uid)
	return nil
}
