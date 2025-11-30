package keycard

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	lp5662DefaultDevice  = "/dev/i2c-2"
	lp5662DefaultAddress = 0x30

	// LP5662 registers
	lp5662RegEnable      = 0x00
	lp5662RegMiscConfig  = 0x01
	lp5662RegPWMBase     = 0x02 // PWM values for LEDs
	lp5662RegCurrentBase = 0x05 // Current settings
	lp5662RegClockConfig = 0x08
	lp5662RegReset       = 0x0D
	lp5662RegPWMConfig   = 0x70

	// Configuration values
	lp5662EnableChip       = 0x40
	lp5662ResetValue       = 0xFF
	lp5662PWMDirectControl = 0x3F
	lp5662PWMOverI2C       = 0x00
	lp5662InternalClock    = 0x01

	// Default LED current (mA setting)
	lp5662DefaultCurrent = 0x14 // ~10mA per channel
)

// RGB color values
type RGB struct {
	R, G, B uint8
}

var (
	ColorOff    = RGB{0, 0, 0}
	ColorRed    = RGB{255, 0, 0}
	ColorGreen  = RGB{0, 255, 0}
	ColorBlue   = RGB{0, 0, 255}
	ColorYellow = RGB{255, 255, 0}
	ColorAmber  = RGB{255, 191, 0} // Orange/amber color
	ColorWhite  = RGB{255, 255, 255}
)

// LP5662 controls the LP5662 RGB LED driver via I2C
type LP5662 struct {
	mu        sync.Mutex
	fd        int
	logger    *slog.Logger
	address   uint8
	color     RGB // current color for On()
	blinkStop chan struct{}
	blinking  bool
}

// NewLP5662 creates a new LP5662 controller
func NewLP5662(device string, address uint8, logger *slog.Logger) (*LP5662, error) {
	if device == "" {
		device = lp5662DefaultDevice
	}
	if address == 0 {
		address = lp5662DefaultAddress
	}

	fd, err := unix.Open(device, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open I2C device %s: %w", device, err)
	}

	led := &LP5662{
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
		return nil, fmt.Errorf("failed to initialize LP5662: %w", err)
	}

	return led, nil
}

func (l *LP5662) setSlaveAddress() error {
	const i2cSlaveForce = 0x0706 // Force access even if kernel driver is bound
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(l.fd), i2cSlaveForce, uintptr(l.address))
	if errno != 0 {
		return errno
	}
	return nil
}

func (l *LP5662) writeReg(reg, value uint8) error {
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

func (l *LP5662) init() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Reset the chip
	if err := l.writeReg(lp5662RegReset, lp5662ResetValue); err != nil {
		return fmt.Errorf("reset failed: %w", err)
	}

	// Set PWM to direct control mode
	if err := l.writeReg(lp5662RegMiscConfig, lp5662PWMDirectControl); err != nil {
		return fmt.Errorf("misc config failed: %w", err)
	}

	// Control PWM over I2C
	if err := l.writeReg(lp5662RegPWMConfig, lp5662PWMOverI2C); err != nil {
		return fmt.Errorf("PWM config failed: %w", err)
	}

	// Use internal clock
	if err := l.writeReg(lp5662RegClockConfig, lp5662InternalClock); err != nil {
		return fmt.Errorf("clock config failed: %w", err)
	}

	// Enable the chip
	if err := l.writeReg(lp5662RegEnable, lp5662EnableChip); err != nil {
		return fmt.Errorf("enable failed: %w", err)
	}

	// Set default current for all channels
	for i := uint8(0); i < 3; i++ {
		if err := l.writeReg(lp5662RegCurrentBase+i, lp5662DefaultCurrent); err != nil {
			return fmt.Errorf("current config failed: %w", err)
		}
	}

	// Turn off all LEDs initially
	if err := l.setColorLocked(ColorOff); err != nil {
		return fmt.Errorf("initial color set failed: %w", err)
	}

	if l.logger != nil {
		l.logger.Info("LP5662 initialized", "address", fmt.Sprintf("0x%02X", l.address))
	}

	return nil
}

func (l *LP5662) setColorLocked(color RGB) error {
	// LP5662 PWM register order: Yellow(unused), Green, Red
	// We map: R->Red, G->Green, B->Yellow channel (or adjust as needed)
	if err := l.writeReg(lp5662RegPWMBase, color.B); err != nil { // Yellow/Blue channel
		return err
	}
	if err := l.writeReg(lp5662RegPWMBase+1, color.G); err != nil { // Green channel
		return err
	}
	if err := l.writeReg(lp5662RegPWMBase+2, color.R); err != nil { // Red channel
		return err
	}
	return nil
}

// SetColor sets the RGB LED color
func (l *LP5662) SetColor(color RGB) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.setColorLocked(color)
}

// Off turns off the LED
func (l *LP5662) Off() error {
	return l.SetColor(ColorOff)
}

// Red sets the LED to red
func (l *LP5662) Red() error {
	return l.SetColor(ColorRed)
}

// Green sets the LED to green
func (l *LP5662) Green() error {
	return l.SetColor(ColorGreen)
}

// Blue sets the LED to blue
func (l *LP5662) Blue() error {
	return l.SetColor(ColorBlue)
}

// Amber sets the LED to amber/orange
func (l *LP5662) Amber() error {
	return l.SetColor(ColorAmber)
}

// Yellow sets the LED to yellow
func (l *LP5662) Yellow() error {
	return l.SetColor(ColorYellow)
}

// On turns on the LED with the configured color
func (l *LP5662) On() error {
	return l.SetColor(l.color)
}

// Flash turns on the LED briefly
func (l *LP5662) Flash(duration time.Duration) {
	l.On()
	time.AfterFunc(duration, func() {
		l.Off()
	})
}

// StartBlink starts blinking the LED
func (l *LP5662) StartBlink(interval time.Duration) {
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
func (l *LP5662) StopBlink() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.blinking {
		return
	}

	close(l.blinkStop)
	l.blinking = false
}

// Close releases the I2C device
func (l *LP5662) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Turn off before closing
	l.setColorLocked(ColorOff)

	return unix.Close(l.fd)
}
