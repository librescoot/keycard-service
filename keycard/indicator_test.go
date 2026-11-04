package keycard

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

type indicatorRegisters struct {
	mu     sync.Mutex
	values [256]byte
	writes int
}

func (r *indicatorRegisters) color() LEDColor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return LEDColor{r.values[2], r.values[3], r.values[4]}
}
func testIndicator(t *testing.T) (*LP5562, *indicatorRegisters) {
	t.Helper()
	regs := &indicatorRegisters{}
	l := &LP5562{fd: -1, color: ColorGreen, writeRegister: func(reg, value uint8) error {
		regs.mu.Lock()
		defer regs.mu.Unlock()
		regs.values[reg] = value
		regs.writes++
		return nil
	}}
	t.Cleanup(func() { _ = l.Close() })
	return l, regs
}
func finishFeedback(l *LP5562) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.feedbackTimer != nil {
		l.indicator.feedbackTimer.Stop()
	}
	l.releaseFeedbackLocked(l.indicator.feedbackGeneration)
}
func wantColor(t *testing.T, r *indicatorRegisters, want LEDColor) {
	t.Helper()
	if got := r.color(); got != want {
		t.Fatalf("color=%+v want=%+v", got, want)
	}
}

func TestAlarmBackgroundYieldsAndRestores(t *testing.T) {
	l, r := testIndicator(t)
	l.SetAlarmIndicator(true)
	wantColor(t, r, alarmPulseColor)
	_ = l.Green()
	l.ReleaseAfter(time.Hour)
	wantColor(t, r, ColorGreen)
	finishFeedback(l)
	wantColor(t, r, alarmPulseColor)
	_ = l.Green()
	l.ReleaseAfter(time.Hour)
	l.SetAlarmIndicator(false)
	wantColor(t, r, ColorGreen)
	finishFeedback(l)
	wantColor(t, r, ColorOff)
}
func TestAlarmDoesNotOverrideEnrollmentOrFault(t *testing.T) {
	l, r := testIndicator(t)
	l.SetAlarmIndicator(true)
	l.SetFeedbackMode("learn")
	wantColor(t, r, ColorAmber)
	l.Flash(time.Hour)
	wantColor(t, r, ColorGreen)
	finishFeedback(l)
	wantColor(t, r, ColorAmber)
	l.SetNFCFault(true)
	wantColor(t, r, ColorRed)
	l.SetAlarmIndicator(false)
	wantColor(t, r, ColorRed)
	l.SetNFCFault(false)
	wantColor(t, r, ColorAmber)
	l.SetFeedbackMode("idle")
	wantColor(t, r, ColorOff)
}
func TestStaleFeedbackCannotEraseNewIndication(t *testing.T) {
	l, r := testIndicator(t)
	l.Flash(time.Hour)
	l.mu.Lock()
	old := l.indicator.feedbackGeneration
	l.mu.Unlock()
	_ = l.Amber()
	l.mu.Lock()
	l.releaseFeedbackLocked(old)
	l.mu.Unlock()
	wantColor(t, r, ColorAmber)
	l.SetFeedbackMode("master-teach-in")
	l.Flash(time.Hour)
	finishFeedback(l)
	wantColor(t, r, ColorAmber)
}
func TestAlarmTimerCannotOverwriteCardFeedback(t *testing.T) {
	l, r := testIndicator(t)
	l.SetAlarmIndicator(true)
	_ = l.Green()
	time.Sleep(alarmPulseOn + 30*time.Millisecond)
	wantColor(t, r, ColorGreen)
	_ = l.Close()
	r.mu.Lock()
	writes := r.writes
	r.mu.Unlock()
	l.SetAlarmIndicator(false)
	l.Flash(time.Millisecond)
	l.StartBlink(time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writes != writes {
		t.Fatal("LED wrote after Close")
	}
}
func TestAlarmIndicatorGate(t *testing.T) {
	for _, tc := range []struct {
		setting, alarm, vehicle string
		want                    bool
	}{
		{"true", "armed", "stand-by", true},
		{"", "armed", "stand-by", false},
		{"false", "armed", "stand-by", false},
		{"true", "delay-armed", "stand-by", false},
		{"true", "disarmed", "stand-by", false},
		{"true", "level-2-triggered", "stand-by", false},
		{"true", "armed", "parked", false},
		{"true", "armed", "ready-to-drive", false},
		{"true", "armed", "", false},
	} {
		if got := alarmIndicatorEnabled(tc.setting, tc.alarm, tc.vehicle); got != tc.want {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
}
func TestAlarmIndicatorRedisStartupAndUpdates(t *testing.T) {
	server := miniredis.RunT(t)
	server.HSet("settings", alarmLEDSetting, "true")
	server.HSet("alarm", "status", "armed")
	server.HSet("vehicle", "state", "stand-by")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := NewRedisClient(server.Addr(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	l, _ := testIndicator(t)
	s := &Service{redis: client, rgbLed: l, logger: logger}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.watchAlarmIndicator(ctx); close(done) }()
	wait := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			l.mu.Lock()
			got := l.indicator.alarm
			l.mu.Unlock()
			if got == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("alarm LED did not become %v", want)
	}
	wait(true)
	server.HSet("vehicle", "state", "parked")
	server.Publish("vehicle", "state")
	wait(false)
	server.HSet("vehicle", "state", "stand-by")
	server.Publish("vehicle", "state")
	wait(true)
	server.HSet("settings", alarmLEDSetting, "false")
	server.Publish("settings", alarmLEDSetting)
	wait(false)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("LED watcher did not stop")
	}
}
