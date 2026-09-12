package keycard

import (
	"fmt"
	"log/slog"
	"os/exec"
	"time"
)

const (
	ledControlScript = "/usr/bin/ledcontrol.sh"

	LedModeLinearOn  = 2
	LedModeLinearOff = 3
	LedModeBlink     = 10

	Led3 = 3
	Led7 = 7
)

// RGBLed controls RGB card-feedback LEDs. Implementations may safely do
// nothing when no LP5562 device is available.
type RGBLed interface {
	On() error
	Off() error
	Flash(duration time.Duration)
	StartBlink(interval time.Duration)
	StopBlink()
	Close() error
	Red() error
	Green() error
	Amber() error
}

// noOpRGBLed intentionally disables RGB card feedback when no usable LP5562
// device is configured. It has no state or background work.
type noOpRGBLed struct{}

func (noOpRGBLed) On() error                { return nil }
func (noOpRGBLed) Off() error               { return nil }
func (noOpRGBLed) Flash(time.Duration)      {}
func (noOpRGBLed) StartBlink(time.Duration) {}
func (noOpRGBLed) StopBlink()               {}
func (noOpRGBLed) Close() error             { return nil }
func (noOpRGBLed) Red() error               { return nil }
func (noOpRGBLed) Green() error             { return nil }
func (noOpRGBLed) Amber() error             { return nil }

// LEDController controls turn-signal indicators for learn mode.
type LEDController struct {
	logger *slog.Logger
}

func NewLEDController(logger *slog.Logger) *LEDController {
	return &LEDController{
		logger: logger,
	}
}

func (l *LEDController) Pattern(led, mode int) {
	l.execScript(ledControlScript, fmt.Sprintf("%d", led), fmt.Sprintf("%d", mode))
}

func (l *LEDController) LedLinearOn(led int) {
	l.Pattern(led, LedModeLinearOn)
}

func (l *LEDController) LedLinearOff(led int) {
	l.Pattern(led, LedModeLinearOff)
}

func (l *LEDController) LedBlink(led int) {
	l.Pattern(led, LedModeBlink)
}

func (l *LEDController) execScript(script string, args ...string) {
	cmd := exec.Command(script, args...)
	if err := cmd.Run(); err != nil {
		l.logger.Warn("LED script failed", "script", script, "args", args, "error", err)
	}
}
