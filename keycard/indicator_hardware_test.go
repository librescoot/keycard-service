package keycard

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt-in: keycard-service must be stopped and the dashboard blinker mirror
// inactive. Only the LP5562 is accessed; no NFC, credentials or vehicle commands.
func TestHardwareAlarmIndicator(t *testing.T) {
	device := os.Getenv("KEYCARD_LED_TEST_DEVICE")
	if device == "" {
		t.Skip("set KEYCARD_LED_TEST_DEVICE with exclusive LED access")
	}
	bus := strings.TrimPrefix(device, "/dev/i2c-")
	if _, err := strconv.Atoi(bus); err != nil {
		t.Fatal("expected /dev/i2c-N")
	}
	read := func(reg uint8) uint8 {
		t.Helper()
		output, err := exec.Command("i2cget", "-f", "-y", bus, "0x30", fmt.Sprintf("0x%02x", reg)).CombinedOutput()
		if err != nil {
			t.Fatalf("read %02x: %v %s", reg, err, output)
		}
		value, err := strconv.ParseUint(strings.TrimSpace(string(output)), 0, 8)
		if err != nil {
			t.Fatal(err)
		}
		return uint8(value)
	}
	check := func(reg, want uint8) {
		t.Helper()
		if got := read(reg); got != want {
			t.Fatalf("register %02x=%02x want %02x", reg, got, want)
		}
	}
	l, err := NewLP5562(device, 0x30, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		l.SetAlarmIndicator(false)
		_ = l.Close()
		// Also clean up if an assertion failed after the poweroff handoff closed l.
		reset, err := NewLP5562(device, 0x30, nil)
		if err != nil {
			t.Errorf("LED cleanup: %v", err)
			return
		}
		if err := reset.Close(); err != nil {
			t.Errorf("LED cleanup close: %v", err)
		}
	}()
	l.SetAlarmIndicator(true)
	check(0, 0xc2)
	check(1, 0x3e)
	check(0x70, 0x30)
	// Program RAM is accessible only in LOAD mode (TI section 7.5.1).
	l.mu.Lock()
	err = l.haltProgramLocked()
	if err == nil {
		err = l.writeReg(1, lp5562Engine3Load)
	}
	l.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Microsecond)
	for i, word := range alarmProgram {
		check(uint8(0x50+i*2), uint8(word>>8))
		check(uint8(0x51+i*2), uint8(word))
	}
	l.mu.Lock()
	err = l.writeReg(1, lp5562Engine3Run)
	time.Sleep(200 * time.Microsecond)
	if err == nil {
		err = l.writeReg(0, lp5562EnableEngine3)
	}
	l.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// PC is specified as readable in HOLD. Let the current longest WAIT
	// finish before reading it, then resume without resetting the program.
	pcs := map[uint8]bool{}
	for i := 0; i < 4; i++ {
		time.Sleep(200 * time.Millisecond)
		l.mu.Lock()
		err = l.writeReg(0, 0xc0)
		l.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond)
		pc := read(lp5562RegEngine3PC)
		pcs[pc] = true
		t.Logf("autonomous PC=%d", pc)
		l.mu.Lock()
		err = l.writeReg(0, 0xc2)
		l.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(pcs) < 2 {
		t.Fatalf("program did not advance: %v", pcs)
	}
	if err = l.Green(); err != nil {
		t.Fatal(err)
	}
	check(0, 0xc0)
	check(1, 0x3f)
	check(0x70, 0)
	check(3, 255)
	check(4, 0)
	l.ReleaseAfter(50 * time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for {
		// Wait for the asynchronous release and its register sequence, not
		// a fixed scheduling margin on the target's single ARM core.
		value := func() uint8 { l.mu.Lock(); defer l.mu.Unlock(); return read(0) }()
		if value == 0xc2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("foreground release did not restore the alarm program")
		}
		time.Sleep(10 * time.Millisecond)
	}
	check(0, 0xc2)
	check(0x70, 0x30)
	l.SetFeedbackMode("learn")
	check(0x70, 0)
	check(2, 255)
	l.SetFeedbackMode("idle")
	check(0x70, 0x30)
	l.SetAlarmIndicator(false)
	check(0x70, 0)
	check(4, 0)
	l.SetAlarmIndicator(true)
	l.SetPowerState("hibernating")
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	check(0, 0xc2)
	check(0x70, 0x30)
	// Simulate the next service startup and leave the LED off.
	fresh, err := NewLP5562(device, 0x30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = fresh.Close(); err != nil {
		t.Fatal(err)
	}
	check(0x70, 0)
	check(4, 0)
	t.Log("autonomous program, foreground preemption, restoration and poweroff handoff passed")
}
