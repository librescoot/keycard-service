package keycard

import "time"

const (
	lp5562RegEngine3PC      = 0x0b
	lp5562RegEngine3Program = 0x50
	lp5562Engine3Load       = 0x3d
	lp5562Engine3Run        = 0x3e
	lp5562RedEngine3        = 0x30
	lp5562EnableEngine3     = 0xc2
)

// TI LP5562 sections 7.3.3.4 and 7.5.1: SET PWM, WAIT (1/64s ticks),
// SET PWM zero, two long WAITs, GO TO START. About 94ms on per 2s cycle.
var alarmProgram = [...]uint16{0x4050, 0x4600, 0x4000, 0x7d00, 0x7d00, 0x0000}

func (l *LP5562) haltProgramLocked() error {
	if err := l.writeReg(lp5562RegEnable, lp5562EnableChip); err != nil {
		return err
	}
	time.Sleep(500 * time.Microsecond)
	if err := l.writeReg(lp5562RegMiscConfig, 0); err != nil {
		return err
	}
	time.Sleep(200 * time.Microsecond)
	return nil
}

func (l *LP5562) startAlarmProgramLocked() (err error) {
	if err = l.applyColorLocked(ColorOff); err != nil {
		return err
	}
	l.indicator.autonomous = true
	defer func() {
		if err != nil {
			// A partial program must not run. A later foreground request retries
			// the stop if even this cleanup cannot communicate with the chip.
			l.logIfErr(l.applyColorLocked(ColorOff))
		}
	}()
	if err = l.writeReg(lp5562RegMiscConfig, lp5562Engine3Load); err != nil {
		return err
	}
	time.Sleep(200 * time.Microsecond)
	for i := 0; i < 16; i++ {
		var word uint16
		if i < len(alarmProgram) {
			word = alarmProgram[i]
		}
		if err = l.writeReg(lp5562RegEngine3Program+uint8(i*2), uint8(word>>8)); err != nil {
			return err
		}
		if err = l.writeReg(lp5562RegEngine3Program+uint8(i*2+1), uint8(word)); err != nil {
			return err
		}
	}
	if err = l.writeReg(lp5562RegEngine3PC, 0); err != nil {
		return err
	}
	time.Sleep(200 * time.Microsecond)
	if err = l.writeReg(lp5562RegPWMConfig, lp5562RedEngine3); err != nil {
		return err
	}
	if err = l.writeReg(lp5562RegMiscConfig, lp5562Engine3Run); err != nil {
		return err
	}
	time.Sleep(200 * time.Microsecond)
	if err = l.writeReg(lp5562RegEnable, lp5562EnableEngine3); err != nil {
		return err
	}
	time.Sleep(500 * time.Microsecond)
	return nil
}

func (l *LP5562) SetPowerState(state string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.closed || l.indicator.powerState == state {
		return
	}
	l.indicator.powerState = state
	if l.indicator.alarm && !l.indicator.fault && l.indicator.mode == "" && enteringLowPower(state) {
		// A host-timed flash cannot expire while the CPU sleeps. Hand back to
		// the background before sleep rather than freezing a transient colour.
		l.cancelFeedbackLocked()
		l.indicator.foreground = false
		l.indicator.blink = 0
	}
	if !l.indicator.foreground {
		l.logIfErr(l.renderLocked())
	}
}
