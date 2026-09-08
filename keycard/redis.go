package keycard

import (
	"fmt"
	"log/slog"
	"time"

	ipc "github.com/librescoot/redis-ipc"
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
