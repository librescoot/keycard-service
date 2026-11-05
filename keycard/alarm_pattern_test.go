package keycard

import (
	"errors"
	"testing"
	"time"
)

func wantAlarmProgram(t *testing.T, l *LP5562, r *indicatorRegisters) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.indicator.autonomous || l.indicator.patternTimer != nil {
		t.Fatal("alarm pattern depends on a host timer")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for reg, want := range map[byte]byte{0: 0xc2, 1: 0x3e, 0x70: 0x30, 0x08: 0x61} {
		if r.values[reg] != want {
			t.Fatalf("register %02x=%02x want %02x", reg, r.values[reg], want)
		}
	}
	for i, word := range alarmProgram {
		got := uint16(r.values[0x50+i*2])<<8 | uint16(r.values[0x51+i*2])
		if got != word {
			t.Fatalf("program word %d=%04x want %04x", i, got, word)
		}
	}
}
func TestAutonomousAlarmNeedsNoPeriodicWrites(t *testing.T) {
	l, r := testIndicator(t)
	l.SetAlarmIndicator(true)
	wantAlarmProgram(t, l, r)
	r.mu.Lock()
	writes := r.writes
	r.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	l.SetAlarmIndicator(true)
	r.mu.Lock()
	got := r.writes
	r.mu.Unlock()
	if got != writes {
		t.Fatal("autonomous pulse rewrote hardware without a state change")
	}
	_ = l.Green()
	r.mu.Lock()
	mode, mapping, enable := r.values[1], r.values[0x70], r.values[0]
	r.mu.Unlock()
	if mode != 0x3f || mapping != 0 || enable != 0xc0 {
		t.Fatal("card feedback did not stop autonomous output")
	}
	wantColor(t, r, ColorGreen)
	_ = l.Off()
	wantAlarmProgram(t, l, r)
	l.SetAlarmIndicator(false)
	wantColor(t, r, ColorOff)
}
func TestPowerTransitionEndsTemporaryFeedbackAndReprogramsOnResume(t *testing.T) {
	l, r := testIndicator(t)
	l.SetPowerState("running")
	l.SetAlarmIndicator(true)
	l.Flash(time.Hour)
	l.mu.Lock()
	old := l.indicator.feedbackGeneration
	l.mu.Unlock()
	l.SetPowerState("suspending-imminent")
	wantAlarmProgram(t, l, r)
	l.mu.Lock()
	l.releaseFeedbackLocked(old)
	l.mu.Unlock()
	wantAlarmProgram(t, l, r)
	// Simulate loss of chip register state while asleep.
	r.mu.Lock()
	r.values = [256]byte{}
	r.mu.Unlock()
	l.SetPowerState("running")
	wantAlarmProgram(t, l, r)
	l.SetFeedbackMode("learn")
	l.SetPowerState("hibernating-manual-imminent")
	wantColor(t, r, ColorAmber)
}
func TestFailedProgramLoadDoesNotLeaveAnEngineRunning(t *testing.T) {
	l, r := testIndicator(t)
	write := l.writeRegister
	l.writeRegister = func(reg, value uint8) error {
		if reg == 0x50 {
			return errors.New("program write failed")
		}
		return write(reg, value)
	}
	l.SetAlarmIndicator(true)
	l.mu.Lock()
	active := l.indicator.autonomous
	l.mu.Unlock()
	if active {
		t.Fatal("partial program remains active")
	}
	wantColor(t, r, ColorOff)
	if err := l.Green(); err != nil {
		t.Fatal(err)
	}
	wantColor(t, r, ColorGreen)
}
func TestShutdownOnlyPreservesAlarmForActualHibernation(t *testing.T) {
	for _, power := range []string{"running", "suspending", "hibernating-imminent", "hibernating", "hibernating-manual", "hibernating-timer", "hibernating-for"} {
		t.Run(power, func(t *testing.T) {
			l, r := testIndicator(t)
			l.SetAlarmIndicator(true)
			l.SetPowerState(power)
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			r.mu.Lock()
			enabled := r.values[0]&3 == 2
			r.mu.Unlock()
			if enabled != poweringOff(power) {
				t.Fatalf("engine running=%v for %s", enabled, power)
			}
		})
	}
}

func TestLowPowerIndicatorEligibility(t *testing.T) {
	powers := []string{"running", "suspending-pending", "suspending-imminent", "suspending", "hibernating", "hibernating-manual-imminent", "hibernating-timer-pending", "hibernating-for"}
	for _, power := range powers {
		for _, vehicle := range []string{"stand-by", "waiting-hibernation", "waiting-hibernation-advanced", "waiting-hibernation-seatbox", "waiting-hibernation-confirm"} {
			if !alarmIndicatorEnabled("true", "true", "armed", vehicle, power) {
				t.Fatalf("disabled for %s/%s", vehicle, power)
			}
		}
		for _, vehicle := range []string{"parked", "ready-to-drive", "hop-on", "waiting-seatbox", "updating", "shutting-down", ""} {
			if alarmIndicatorEnabled("true", "true", "armed", vehicle, power) {
				t.Fatalf("enabled for %s/%s", vehicle, power)
			}
		}
	}
	for _, power := range []string{"", "unknown", "reboot", "reboot-imminent"} {
		if alarmIndicatorEnabled("true", "true", "armed", "stand-by", power) {
			t.Fatalf("enabled with power=%q", power)
		}
	}
}
