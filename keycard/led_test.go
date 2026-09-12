package keycard

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

type rgbLEDTestFaultReporter struct{}

func (rgbLEDTestFaultReporter) RaiseNFCUnavailableFault(string) error { return nil }
func (rgbLEDTestFaultReporter) ClearNFCUnavailableFault() error       { return nil }

func newRGBLEDTestService(t *testing.T, ledDevice string, logger *slog.Logger) *Service {
	t.Helper()

	server := miniredis.RunT(t)
	service, err := NewService(&Config{
		DataDir:    t.TempDir(),
		RedisAddr:  server.Addr(),
		LEDDevice:  ledDevice,
		LEDAddress: 0x30,
	}, logger)
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	t.Cleanup(func() {
		service.cancel()
		_ = service.redis.Close()
	})
	return service
}

func TestNewServiceDisablesRGBFeedbackWithoutUsableLP5562(t *testing.T) {
	tests := []struct {
		name      string
		ledDevice string
	}{
		{name: "not configured"},
		{name: "initialization fails", ledDevice: "/dev/not-an-i2c-device"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			service := newRGBLEDTestService(t, test.ledDevice, logger)

			if _, ok := service.rgbLed.(noOpRGBLed); !ok {
				t.Fatalf("rgbLed = %T, want noOpRGBLed", service.rgbLed)
			}
			if !strings.Contains(logs.String(), "RGB feedback disabled") {
				t.Fatalf("initialization log = %q, want RGB feedback disabled", logs.String())
			}
			if strings.Contains(logs.String(), "script") {
				t.Fatalf("initialization log mentions a script: %q", logs.String())
			}
		})
	}
}

func TestNFCFaultWithNoOpRGBLEDDoesNotRunScriptsOrBackgroundBlinking(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	service := newRGBLEDTestService(t, "/dev/not-an-i2c-device", logger)
	service.faults = rgbLEDTestFaultReporter{}
	logs.Reset()

	service.handleNFCFailure(errors.New("reader unavailable"))
	service.handleNFCFailure(errors.New("reader unavailable"))

	if !service.nfcFaultActive {
		t.Fatal("NFC fault indication was not marked active")
	}
	if strings.Contains(logs.String(), "LED script failed") {
		t.Fatalf("fault indication ran an LED script: %q", logs.String())
	}

	// The no-op implementation has no state or goroutine to stop. Repeated
	// calls must remain immediately inert while a fault remains active.
	done := make(chan struct{})
	go func() {
		for range 10_000 {
			service.rgbLed.StartBlink(time.Nanosecond)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("no-op RGB blink did not return promptly")
	}
}

func TestNoOpRGBLEDMethodsAreSafe(t *testing.T) {
	led := noOpRGBLed{}
	if err := led.On(); err != nil {
		t.Fatal(err)
	}
	if err := led.Red(); err != nil {
		t.Fatal(err)
	}
	if err := led.Green(); err != nil {
		t.Fatal(err)
	}
	if err := led.Amber(); err != nil {
		t.Fatal(err)
	}
	led.Flash(time.Nanosecond)
	led.StartBlink(time.Nanosecond)
	led.StopBlink()
	if err := led.Off(); err != nil {
		t.Fatal(err)
	}
	if err := led.Close(); err != nil {
		t.Fatal(err)
	}
}
