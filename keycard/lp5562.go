package keycard

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	lp5562DefaultDevice  = "/dev/i2c-2"
	lp5562DefaultAddress = 0x30

	lp5562RegEnable      = 0x00
	lp5562RegMiscConfig  = 0x01
	lp5562RegPWMBase     = 0x02 // PWM0..2 are amber, green, red.
	lp5562RegCurrentBase = 0x05
	lp5562RegClockConfig = 0x08
	lp5562RegReset       = 0x0D
	lp5562RegPWMConfig   = 0x70

	lp5562EnableChip       = 0xC0 // bit6=CHIP_EN, bit7=logarithmic PWM
	lp5562ResetValue       = 0xFF
	lp5562PWMDirectControl = 0x3F
	lp5562PWMOverI2C       = 0x00
	lp5562InternalClock    = 0x61 // Internal clock, power save, high-frequency PWM.

	lp5562DefaultCurrent = 0xAF // 17.5 mA at 100 microamps per step.
)

// LP5562 channel order is amber, green, red.
type LEDColor struct {
	Amber, Green, Red uint8
}

var (
	ColorOff   = LEDColor{0, 0, 0}
	ColorRed   = LEDColor{0, 0, 255}
	ColorGreen = LEDColor{0, 255, 0}
	ColorAmber = LEDColor{255, 0, 0}
)

type LP5562 struct {
	mu            sync.Mutex
	fd            int
	logger        *slog.Logger
	address       uint8
	color         LEDColor
	indicator     indicatorState
	writeRegister func(uint8, uint8) error // Optional transport for register-level tests.
}

func NewLP5562(device string, address uint8, logger *slog.Logger) (*LP5562, error) {
	if device == "" {
		device = lp5562DefaultDevice
	}
	if address == 0 {
		address = lp5562DefaultAddress
	}

	fd, err := unix.Open(device, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open I2C device %s: %w", device, err)
	}

	led := &LP5562{
		fd:      fd,
		logger:  logger,
		address: address,
		color:   ColorGreen,
	}

	if err := led.setSlaveAddress(); err != nil {
		if cerr := unix.Close(fd); cerr != nil && logger != nil {
			logger.Warn("Failed to close I2C device", "device", device, "error", cerr)
		}
		return nil, fmt.Errorf("failed to set I2C address: %w", err)
	}

	if err := led.init(); err != nil {
		if cerr := unix.Close(fd); cerr != nil && logger != nil {
			logger.Warn("Failed to close I2C device", "device", device, "error", cerr)
		}
		return nil, fmt.Errorf("failed to initialize LP5562: %w", err)
	}

	return led, nil
}

func (l *LP5562) setSlaveAddress() error {
	const i2cSlaveForce = 0x0706 // Force access even if kernel driver is bound
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(l.fd), i2cSlaveForce, uintptr(l.address))
	if errno != 0 {
		return errno
	}
	return nil
}

func (l *LP5562) writeReg(reg, value uint8) error {
	if l.writeRegister != nil {
		return l.writeRegister(reg, value)
	}
	buf := []byte{reg, value}
	n, err := unix.Write(l.fd, buf)
	if err != nil {
		return err
	}
	if n != 2 {
		return fmt.Errorf("short write: %d", n)
	}
	return nil
}

// applyConfigLocked writes the operating-mode, clock, enable and drive-current
// registers. When the optional DBC blinker LED setting is enabled,
// vehicle-service writes to this LP5562 (also through I2C_SLAVE_FORCE) and
// its off frame changes the drive current and clock configuration. Re-assert
// our settings before each colour change.
func (l *LP5562) applyConfigLocked() error {
	if err := l.writeReg(lp5562RegMiscConfig, lp5562PWMDirectControl); err != nil {
		return fmt.Errorf("misc config failed: %w", err)
	}
	time.Sleep(200 * time.Microsecond)

	if err := l.writeReg(lp5562RegPWMConfig, lp5562PWMOverI2C); err != nil {
		return fmt.Errorf("PWM config failed: %w", err)
	}

	if err := l.writeReg(lp5562RegClockConfig, lp5562InternalClock); err != nil {
		return fmt.Errorf("clock config failed: %w", err)
	}

	if err := l.writeReg(lp5562RegEnable, lp5562EnableChip); err != nil {
		return fmt.Errorf("enable failed: %w", err)
	}
	time.Sleep(500 * time.Microsecond)

	for i := uint8(0); i < 3; i++ {
		if err := l.writeReg(lp5562RegCurrentBase+i, lp5562DefaultCurrent); err != nil {
			return fmt.Errorf("current config failed: %w", err)
		}
	}

	return nil
}

func (l *LP5562) init() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.writeReg(lp5562RegReset, lp5562ResetValue); err != nil {
		return fmt.Errorf("reset failed: %w", err)
	}
	time.Sleep(time.Millisecond)

	if err := l.applyConfigLocked(); err != nil {
		return err
	}

	if err := l.setColorLocked(ColorOff); err != nil {
		return fmt.Errorf("initial color set failed: %w", err)
	}

	if l.logger != nil {
		l.logger.Info("LP5562 initialized", "address", fmt.Sprintf("0x%02X", l.address))
	}

	return nil
}

func (l *LP5562) setColorLocked(color LEDColor) error {
	// LP5562 PWM register order: Amber (PWM0), Green (PWM1), Red (PWM2)
	if err := l.writeReg(lp5562RegPWMBase, color.Amber); err != nil {
		return err
	}
	if err := l.writeReg(lp5562RegPWMBase+1, color.Green); err != nil {
		return err
	}
	if err := l.writeReg(lp5562RegPWMBase+2, color.Red); err != nil {
		return err
	}
	return nil
}

func (l *LP5562) applyColorLocked(color LEDColor) error {
	if l.indicator.autonomous {
		if err := l.haltProgramLocked(); err != nil {
			return err
		}
		l.indicator.autonomous = false
	}
	if err := l.applyConfigLocked(); err != nil {
		return err
	}
	return l.setColorLocked(color)
}

func (l *LP5562) logIfErr(err error) {
	if err != nil && l.logger != nil {
		l.logger.Warn("LP5562 I2C write failed", "error", err)
	}
}

func (l *LP5562) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.closed {
		return nil
	}
	l.cancelFeedbackLocked()
	l.cancelPatternLocked()
	l.indicator.closed = true
	keepAlarm := l.indicator.autonomous && l.indicator.alarm &&
		!l.indicator.foreground && !l.indicator.fault && l.indicator.mode == "" &&
		poweringOff(l.indicator.powerState)
	if !keepAlarm {
		l.logIfErr(l.applyColorLocked(ColorOff))
	}
	if l.fd < 0 {
		return nil
	}
	err := unix.Close(l.fd)
	l.fd = -1
	return err
}
