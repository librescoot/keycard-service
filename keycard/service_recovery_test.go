package keycard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	hal "github.com/librescoot/pn7150"
)

type recoveryTestNFC struct {
	startErrors []error
	awaitError  error
	started     chan struct{}
	unblock     chan struct{}

	mu     sync.Mutex
	starts int
	closed int
}

func (n *recoveryTestNFC) Initialize() error { return nil }
func (n *recoveryTestNFC) StartDiscovery(uint) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.starts++
	if n.started != nil {
		select {
		case <-n.started:
		default:
			close(n.started)
		}
	}
	if i := n.starts - 1; i < len(n.startErrors) {
		return n.startErrors[i]
	}
	return nil
}
func (n *recoveryTestNFC) StopDiscovery() error    { return nil }
func (n *recoveryTestNFC) FullReinitialize() error { return nil }
func (n *recoveryTestNFC) AwaitReadable(time.Duration) error {
	if n.unblock != nil {
		<-n.unblock
	}
	return n.awaitError
}
func (n *recoveryTestNFC) DetectTags() ([]hal.Tag, error) { return nil, nil }
func (n *recoveryTestNFC) SelectTag(uint) error           { return nil }
func (n *recoveryTestNFC) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed++
}

type recoveryTestLED struct {
	red, amber, starts, stops int
}

func (l *recoveryTestLED) On() error                { return nil }
func (l *recoveryTestLED) Off() error               { return nil }
func (l *recoveryTestLED) Flash(time.Duration)      {}
func (l *recoveryTestLED) StartBlink(time.Duration) { l.starts++ }
func (l *recoveryTestLED) StopBlink()               { l.stops++ }
func (l *recoveryTestLED) Close() error             { return nil }
func (l *recoveryTestLED) Red() error               { l.red++; return nil }
func (l *recoveryTestLED) Green() error             { return nil }
func (l *recoveryTestLED) Amber() error             { l.amber++; return nil }

type recoveryTestFaults struct {
	mu     sync.Mutex
	raises []string
	clears int
}

func (f *recoveryTestFaults) RaiseNFCUnavailableFault(description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raises = append(f.raises, description)
	return nil
}
func (f *recoveryTestFaults) ClearNFCUnavailableFault() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clears++
	return nil
}

func newRecoveryTestService(t *testing.T) *Service {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth, err := NewAuthManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SetMaster("AABBCCDD"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	led := NewLEDController(logger)
	return &Service{
		config:           &Config{Device: "/dev/does-not-matter"},
		logger:           logger,
		auth:             auth,
		rgbLed:           led,
		blinkerLed:       led,
		ctx:              ctx,
		cancel:           cancel,
		done:             make(chan struct{}),
		watchCommands:    func(context.Context) {},
		waitForReconnect: func(time.Duration) bool { return true },
	}
}

func runUntilStarted(t *testing.T, service *Service, nfc *recoveryTestNFC) chan error {
	t.Helper()
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run() }()
	select {
	case <-nfc.started:
	case <-time.After(time.Second):
		t.Fatal("service did not start NFC polling")
	}
	return runDone
}

func stopRecoveryTestService(t *testing.T, service *Service, nfc *recoveryTestNFC, runDone chan error) {
	t.Helper()
	service.cancel()
	close(nfc.unblock)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not stop")
	}
}

func TestNFCFailureBlinksRedUntilRecovery(t *testing.T) {
	service := newRecoveryTestService(t)
	led := &recoveryTestLED{}
	service.rgbLed = led
	service.faults = &recoveryTestFaults{}

	service.handleNFCFailure(errors.New("reader disconnected"))
	service.handleNFCFailure(errors.New("reader disconnected"))
	if led.red != 1 || led.starts != 1 {
		t.Fatalf("fault LED calls = red:%d start:%d, want 1 each", led.red, led.starts)
	}

	service.clearNFCFaultIndication()
	if led.stops != 1 {
		t.Fatalf("fault LED stop calls = %d, want 1", led.stops)
	}
}

func TestRunRetriesMissingNFCAtStartup(t *testing.T) {
	t.Parallel()

	service := newRecoveryTestService(t)
	second := &recoveryTestNFC{
		started:    make(chan struct{}),
		unblock:    make(chan struct{}),
		awaitError: errors.New("read interrupted"),
	}
	attempts := 0
	service.nfcFactory = func(*Config, *slog.Logger) (nfcReader, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("device not found")
		}
		return second, nil
	}
	faults := &recoveryTestFaults{}
	service.faults = faults

	runDone := runUntilStarted(t, service, second)
	stopRecoveryTestService(t, service, second, runDone)

	faults.mu.Lock()
	defer faults.mu.Unlock()
	if len(faults.raises) != 1 {
		t.Fatalf("fault raises = %d, want 1", len(faults.raises))
	}
	if faults.clears != 1 {
		t.Fatalf("fault clears = %d, want 1", faults.clears)
	}
}

func TestRunReconnectsNFCAndTransitionsFault(t *testing.T) {
	t.Parallel()

	service := newRecoveryTestService(t)
	first := &recoveryTestNFC{
		startErrors: []error{nil, errors.New("reader disconnected")},
		awaitError:  errors.New("read failed"),
	}
	second := &recoveryTestNFC{
		started:    make(chan struct{}),
		unblock:    make(chan struct{}),
		awaitError: errors.New("read interrupted"),
	}
	factoryCalls := 0
	service.nfcFactory = func(*Config, *slog.Logger) (nfcReader, error) {
		factoryCalls++
		if factoryCalls == 1 {
			return first, nil
		}
		return second, nil
	}
	faults := &recoveryTestFaults{}
	service.faults = faults

	runDone := runUntilStarted(t, service, second)
	stopRecoveryTestService(t, service, second, runDone)

	faults.mu.Lock()
	defer faults.mu.Unlock()
	if len(faults.raises) != 1 {
		t.Fatalf("fault raises = %d, want 1", len(faults.raises))
	}
	if faults.clears != 2 {
		t.Fatalf("fault clears = %d, want 2 (one for each successful connection)", faults.clears)
	}
	if first.closed != 1 || second.closed != 1 {
		t.Fatalf("reader close calls = first:%d second:%d, want 1 each", first.closed, second.closed)
	}
}
