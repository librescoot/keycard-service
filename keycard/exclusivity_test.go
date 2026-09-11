package keycard

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

type exclusivityTestLED struct {
	red atomic.Int32
}

func (*exclusivityTestLED) On() error                { return nil }
func (*exclusivityTestLED) Off() error               { return nil }
func (*exclusivityTestLED) Flash(time.Duration)      {}
func (*exclusivityTestLED) StartBlink(time.Duration) {}
func (*exclusivityTestLED) StopBlink()               {}
func (*exclusivityTestLED) Close() error             { return nil }
func (l *exclusivityTestLED) Red() error             { l.red.Add(1); return nil }
func (*exclusivityTestLED) Green() error             { return nil }
func (*exclusivityTestLED) Amber() error             { return nil }

func newExclusivityTestService(t *testing.T) (*Service, *miniredis.Miniredis) {
	t.Helper()

	server := miniredis.RunT(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	redis, err := NewRedisClient(server.Addr(), logger)
	if err != nil {
		t.Fatalf("NewRedisClient failed: %v", err)
	}
	t.Cleanup(func() { _ = redis.Close() })

	auth, err := NewAuthManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuthManager failed: %v", err)
	}
	return &Service{
		logger: logger,
		auth:   auth,
		rgbLed: &exclusivityTestLED{},
		redis:  redis,
	}, server
}

func TestCommandMutationsRejectCrossRoleDuplicates(t *testing.T) {
	service, server := newExclusivityTestService(t)
	if err := service.auth.SetMaster(master1); err != nil {
		t.Fatalf("SetMaster failed: %v", err)
	}
	mustAddAuthorized(t, service.auth, user1)

	tests := []struct {
		name   string
		mutate func()
	}{
		{name: "set master to authorized card", mutate: func() { service.handleSetMaster(user1) }},
		{name: "add authorized card as master", mutate: func() { service.handleMasterAdd(user1) }},
		{name: "add master as authorized card", mutate: func() { service.handleAdd(master1) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.mutate()
			if got := server.HGet(keycardHashKey, "command-error"); got != codeAlreadyRegistered {
				t.Errorf("command-error = %q, want %q", got, codeAlreadyRegistered)
			}
			if got := server.HGet(keycardHashKey, "command-result"); got != prose(codeAlreadyRegistered) {
				t.Errorf("command-result = %q, want compatibility prose %q", got, prose(codeAlreadyRegistered))
			}
			if !service.auth.IsMaster(master1) || service.auth.IsMaster(user1) {
				t.Error("rejected command changed master roles")
			}
			if !service.auth.CanUnlock(user1) || service.auth.CanUnlock(master1) {
				t.Error("rejected command changed authorized roles")
			}
		})
	}
}

func TestBootstrapRejectsAuthorizedCard(t *testing.T) {
	service, _ := newExclusivityTestService(t)
	mustAddAuthorized(t, service.auth, user1)
	service.masterBootstrapMode = true

	service.bootstrapMasterUID(user1)

	if !service.masterBootstrapMode {
		t.Error("bootstrap should remain armed after rejecting an authorized card")
	}
	if service.auth.HasMaster() || service.auth.IsMaster(user1) {
		t.Error("bootstrap registered an authorized card as master")
	}
	if !service.auth.CanUnlock(user1) {
		t.Error("bootstrap changed the authorized role")
	}
	led := service.rgbLed.(*exclusivityTestLED)
	if got := led.red.Load(); got != 1 {
		t.Errorf("red feedback count = %d, want 1", got)
	}
}
