package keycard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	redis "github.com/redis/go-redis/v9"
)

const (
	keycardHashKey      = "keycard"
	keycardExpiry       = 10 * time.Second
	keycardEventChannel = "keycard:events"
)

type RedisClient struct {
	client *ipc.Client
	logger *slog.Logger
	faults *ipc.FaultReporter
}

func NewRedisClient(addr string, logger *slog.Logger) (*RedisClient, error) {
	client, err := ipc.New(
		ipc.WithURL(addr),
		ipc.WithLogger(logger),
		ipc.WithCodec(ipc.StringCodec{}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &RedisClient{
		client: client,
		logger: logger,
		faults: client.NewFaultReporter("keycard"),
	}, nil
}

func (r *RedisClient) Close() error {
	return r.client.Close()
}

func (r *RedisClient) RaiseNFCUnavailableFault(description string) error {
	return r.faults.Raise(1, description)
}

func (r *RedisClient) ClearNFCUnavailableFault() error {
	return r.faults.Clear(1)
}

// ServiceModeActive reports whether settings-service publishes its service
// overlay as live. A missing field or read error reads as inactive, so the
// gate built on this can only ever suppress a convenience, not block auth.
func (r *RedisClient) ServiceModeActive() bool {
	value, err := r.client.Hash("settings").Get("dashboard.service-mode-active")
	if err != nil {
		return false
	}
	return value == "true"
}

// PublishKeycardSnapshot replaces the dashboard's credential snapshots and
// notifies set readers after the transaction commits.
func (r *RedisClient) PublishKeycardSnapshot(masters, authorized, phones []string) error {
	if _, err := r.client.Raw().TxPipelined(context.Background(), func(pipe redis.Pipeliner) error {
		pipe.Del(context.Background(), "keycard:authorized", "keycard:masters", "keycard:phones")
		for setName, values := range map[string][]string{
			"keycard:authorized": authorized,
			"keycard:masters":    masters,
			"keycard:phones":     phones,
		} {
			if len(values) == 0 {
				continue
			}
			members := make([]interface{}, len(values))
			for i, value := range values {
				members[i] = value
			}
			pipe.SAdd(context.Background(), setName, members...)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to publish keycard snapshots: %w", err)
	}
	if err := r.PublishKeycardCounts(len(masters), len(authorized)); err != nil {
		return err
	}
	lastUID, err := r.client.Hash("system").Get("keycard-last-used-uid")
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("failed to read last used card: %w", err)
	}
	found := false
	for _, uid := range authorized {
		if uid == lastUID {
			found = true
			break
		}
	}
	if lastUID != "" && !found {
		if err := r.client.Hash("system").Set("keycard-last-used-uid", "", ipc.Sync()); err != nil {
			return fmt.Errorf("failed to clear last used card: %w", err)
		}
	}
	for _, setName := range []string{"keycard:authorized", "keycard:masters", "keycard:phones"} {
		if _, err := r.client.Publish("system", setName); err != nil {
			return fmt.Errorf("failed to publish keycard snapshot notification: %w", err)
		}
	}
	return nil
}

// PublishKeyAliases replaces the dashboard's optional credential names.
func (r *RedisClient) PublishKeyAliases(names map[string]string) error {
	if _, err := r.client.Raw().TxPipelined(context.Background(), func(pipe redis.Pipeliner) error {
		pipe.Del(context.Background(), "keycard:aliases")
		entries := make([]interface{}, 0, len(names))
		for _, key := range sortedAliases(names) {
			entries = append(entries, key+":"+names[key])
		}
		if len(entries) > 0 {
			pipe.SAdd(context.Background(), "keycard:aliases", entries...)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to publish key names: %w", err)
	}
	if _, err := r.client.Publish("system", "keycard:aliases"); err != nil {
		return fmt.Errorf("failed to notify key names: %w", err)
	}
	return nil
}

func (r *RedisClient) PublishProtocolVersion() error {
	if err := r.client.Raw().Set(context.Background(), "keycard:protocol-version", "2", 30*time.Second).Err(); err != nil {
		return fmt.Errorf("failed to advertise keycard protocol: %w", err)
	}
	return nil
}

func (r *RedisClient) PublishLastUsedCard(uid string) error {
	if err := r.client.Hash("system").Set("keycard-last-used-uid", uid, ipc.Sync()); err != nil {
		return fmt.Errorf("failed to publish last used card: %w", err)
	}
	return nil
}

// PublishLearnState persists the active enrollment mode for dashboard startup
// and publishes it through the normal system hash channel.
func (r *RedisClient) PublishLearnState(state string) error {
	if err := r.client.Hash("system").Set("keycard-learn-state", state); err != nil {
		return fmt.Errorf("failed to publish keycard learn state: %w", err)
	}
	return nil
}

// PublishKeycardCounts writes pairing counts to the shared "system" hash.
// The "keycard" hash is reserved for transient auth events with a 10s
// expiry, so persistent ambient state lives elsewhere.
func (r *RedisClient) PublishKeycardCounts(masterCount, authorizedCount int) error {
	_, err := r.client.Hash("system").SetManyIfChanged(map[string]any{
		"keycard-master-count":     masterCount,
		"keycard-authorized-count": authorizedCount,
	})
	if err != nil {
		return fmt.Errorf("failed to publish keycard counts: %w", err)
	}
	return nil
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

	if _, err := r.client.Expire(keycardHashKey, keycardExpiry); err != nil {
		r.logger.Warn("Failed to set expiry on keycard hash", "error", err)
	}

	r.logger.Info("Published authentication", "uid", uid)
	return nil
}

// PublishKeycardEvent publishes a transient event to the keycard:events
// PUBSUB channel. Subscribers (installer, BLE bridge, ...) get real-time
// notifications for every state change, whether a Redis command or a tap on
// the reader caused it. A subscriber that only ever hears about the changes
// it asked for cannot stay in step with the vehicle.
//
// Payloads are colon-separated, "<event>[:<uid>][:<trigger>]", where trigger
// is one of card, command, bootstrap, teach-in.
//
// Modes:
//   - "mode-entered:learn:<trigger>" / "mode-exited:learn:<trigger>"
//   - "mode-entered:master" / "mode-exited:master"  (teach-in; no trigger
//     suffix, this is the payload the installer already matches on)
//   - "mode-entered:master-bootstrap:boot"
//   - "mode-exited:master-bootstrap:<trigger>"
//
// Cards:
//   - "card-learned:<uid>"       (learn-mode tap, queued until learn:stop)
//   - "card-duplicate:<uid>"     (already registered, or already this session)
//   - "card-added:<uid>:command"
//   - "card-removed:<uid>:command"
//   - "access-granted:<uid>"
//
// Masters:
//   - "master-added:<uid>:<trigger>"
//   - "master-learned:<uid>"     (teach-in only; same fact as master-added
//     with the teach-in trigger, kept for the installer)
//   - "master-removed:<uid>:command"
//   - "masters-cleared"
//
// Failures:
//   - "rejected:already-authorized:<uid>"
//   - "error:save-failed:<uid>"
//
// Whole-state:
//   - "reset"
func (r *RedisClient) PublishKeycardEvent(payload string) error {
	if _, err := r.client.Publish(keycardEventChannel, payload); err != nil {
		return fmt.Errorf("failed to publish keycard event: %w", err)
	}
	return nil
}
