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

	lp5562EnableChip       = 0xC0 // bit6=CHIP_EN, bit7=OUTPUT_EN
	lp5562ResetValue       = 0xFF
	lp5562PWMDirectControl = 0x3F
	lp5562PWMOverI2C       = 0x00
	lp5562InternalClock    = 0x61 // bit0=internal clock, bit5+6=PWM clock enable

	lp5562DefaultCurrent = 0xAF // Approximately 175, matching the hardware reference.
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
	mu        sync.Mutex
	fd        int
	logger    *slog.Logger
	address   uint8
	color     LEDColor // Color restored by On after a temporary Off.
	blinkStop chan struct{}
	blinking  bool
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
// registers. vehicle-service drives the same chip for the blinker indicator
// (I2C_SLAVE_FORCE on the same bus and address) and leaves it on a lower drive
// current with logarithmic dimming disabled, so re-assert our own settings
// before every colour change rather than trusting the state we set at init.
func (l *LP5562) applyConfigLocked() error {
	if err := l.writeReg(lp5562RegMiscConfig, lp5562PWMDirectControl); err != nil {
		return fmt.Errorf("misc config failed: %w", err)
	}

	if err := l.writeReg(lp5562RegPWMConfig, lp5562PWMOverI2C); err != nil {
		return fmt.Errorf("PWM config failed: %w", err)
	}

	if err := l.writeReg(lp5562RegClockConfig, lp5562InternalClock); err != nil {
		return fmt.Errorf("clock config failed: %w", err)
	}

	if err := l.writeReg(lp5562RegEnable, lp5562EnableChip); err != nil {
		return fmt.Errorf("enable failed: %w", err)
	}

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

func (l *LP5562) SetColor(color LEDColor) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.applyConfigLocked(); err != nil {
		return err
	}
	return l.setColorLocked(color)
}

func (l *LP5562) Off() error {
	return l.SetColor(ColorOff)
}

func (l *LP5562) Red() error {
	return l.SetColor(ColorRed)
}

func (l *LP5562) Green() error {
	return l.SetColor(ColorGreen)
}

func (l *LP5562) Amber() error {
	return l.SetColor(ColorAmber)
}

func (l *LP5562) On() error {
	return l.SetColor(l.color)
}

// logIfErr logs I2C failures from LED state changes. The scooter keeps
// working with a dark or stuck indicator, so these are surfaced but never
// propagated to the caller.
func (l *LP5562) logIfErr(err error) {
	if err != nil && l.logger != nil {
		l.logger.Warn("LP5562 I2C write failed", "error", err)
	}
}

func (l *LP5562) Flash(duration time.Duration) {
	l.logIfErr(l.On())
	time.AfterFunc(duration, func() {
		l.logIfErr(l.Off())
	})
}

func (l *LP5562) StartBlink(interval time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.blinking {
		return
	}

	l.blinking = true
	l.blinkStop = make(chan struct{})

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		state := false
		for {
			select {
			case <-l.blinkStop:
				l.logIfErr(l.Off())
				return
			case <-ticker.C:
				if state {
					l.logIfErr(l.Off())
				} else {
					l.logIfErr(l.On())
				}
				state = !state
			}
		}
	}()
}

func (l *LP5562) StopBlink() {
	l.mu.Lock()
	if !l.blinking {
		l.mu.Unlock()
		return
	}

	stopChan := l.blinkStop
	l.blinking = false
	l.mu.Unlock()

	close(stopChan)
}

func (l *LP5562) Close() error {
	l.StopBlink()

	l.mu.Lock()
	defer l.mu.Unlock()

	_ = l.setColorLocked(ColorOff)
	return unix.Close(l.fd)
}
