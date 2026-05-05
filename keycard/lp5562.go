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

	// LP5562 registers
	lp5562RegEnable      = 0x00
	lp5562RegMiscConfig  = 0x01
	lp5562RegPWMBase     = 0x02 // PWM values for LEDs
	lp5562RegCurrentBase = 0x05 // Current settings
	lp5562RegClockConfig = 0x08
	lp5562RegReset       = 0x0D
	lp5562RegPWMConfig   = 0x70

	// Configuration values
	lp5562EnableChip       = 0xC0 // bit6=CHIP_EN, bit7=OUTPUT_EN
	lp5562ResetValue       = 0xFF
	lp5562PWMDirectControl = 0x3F
	lp5562PWMOverI2C       = 0x00
	lp5562InternalClock    = 0x61 // bit0=internal clock, bit5+6=PWM clock enable

	// Default LED current
	lp5562DefaultCurrent = 0xAF // ~175 matching reference implementation
)

// LEDColor represents PWM values for the LP5562 tri-color (RAG) LED.
// Channels: Amber (PWM0), Green (PWM1), Red (PWM2).
type LEDColor struct {
	Amber, Green, Red uint8
}

var (
	ColorOff   = LEDColor{0, 0, 0}
	ColorRed   = LEDColor{0, 0, 255}
	ColorGreen = LEDColor{0, 255, 0}
	ColorAmber = LEDColor{255, 0, 0}
)

// LP5562 controls the LP5562 tri-color (RAG) LED driver via I2C
type LP5562 struct {
	mu        sync.Mutex
	fd        int
	logger    *slog.Logger
	address   uint8
	color     LEDColor // current color for On()
	blinkStop chan struct{}
	blinking  bool
}

// NewLP5562 creates a new LP5562 controller
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
		color:   ColorGreen, // default to green for keycard feedback
	}

	if err := led.setSlaveAddress(); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("failed to set I2C address: %w", err)
	}

	if err := led.init(); err != nil {
		unix.Close(fd)
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

func (l *LP5562) init() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Reset the chip
	if err := l.writeReg(lp5562RegReset, lp5562ResetValue); err != nil {
		return fmt.Errorf("reset failed: %w", err)
	}

	// Set PWM to direct control mode
	if err := l.writeReg(lp5562RegMiscConfig, lp5562PWMDirectControl); err != nil {
		return fmt.Errorf("misc config failed: %w", err)
	}

	// Control PWM over I2C
	if err := l.writeReg(lp5562RegPWMConfig, lp5562PWMOverI2C); err != nil {
		return fmt.Errorf("PWM config failed: %w", err)
	}

	// Use internal clock
	if err := l.writeReg(lp5562RegClockConfig, lp5562InternalClock); err != nil {
		return fmt.Errorf("clock config failed: %w", err)
	}

	// Enable the chip
	if err := l.writeReg(lp5562RegEnable, lp5562EnableChip); err != nil {
		return fmt.Errorf("enable failed: %w", err)
	}

	// Set default current for all channels
	for i := uint8(0); i < 3; i++ {
		if err := l.writeReg(lp5562RegCurrentBase+i, lp5562DefaultCurrent); err != nil {
			return fmt.Errorf("current config failed: %w", err)
		}
	}

	// Turn off all LEDs initially
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

// SetColor sets the LED color
func (l *LP5562) SetColor(color LEDColor) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.setColorLocked(color)
}

// Off turns off the LED
func (l *LP5562) Off() error {
	return l.SetColor(ColorOff)
}

// Red sets the LED to red
func (l *LP5562) Red() error {
	return l.SetColor(ColorRed)
}

// Green sets the LED to green
func (l *LP5562) Green() error {
	return l.SetColor(ColorGreen)
}

// Amber sets the LED to amber
func (l *LP5562) Amber() error {
	return l.SetColor(ColorAmber)
}

// On turns on the LED with the configured color
func (l *LP5562) On() error {
	return l.SetColor(l.color)
}

// Flash turns on the LED briefly
func (l *LP5562) Flash(duration time.Duration) {
	l.On()
	time.AfterFunc(duration, func() {
		l.Off()
	})
}

// StartBlink starts blinking the LED
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
				l.Off()
				return
			case <-ticker.C:
				if state {
					l.Off()
				} else {
					l.On()
				}
				state = !state
			}
		}
	}()
}

// StopBlink stops blinking the LED
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

// Close releases the I2C device
func (l *LP5562) Close() error {
	l.StopBlink()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.setColorLocked(ColorOff)
	return unix.Close(l.fd)
}
