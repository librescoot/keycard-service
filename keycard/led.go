package keycard

import (
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

const (
	greenLedScript   = "/usr/bin/greenled.sh"
	ledControlScript = "/usr/bin/ledcontrol.sh"

	LedModeLinearOn  = 2
	LedModeLinearOff = 3
	LedModeBlink     = 10

	Led3 = 3
	Led7 = 7
)

// RGBLed abstracts the LP5562 and script fallback; color methods may be no-ops.
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

type LEDController struct {
	mu        sync.Mutex
	logger    *slog.Logger
	blinkStop chan struct{}
	blinking  bool
}

func NewLEDController(logger *slog.Logger) *LEDController {
	return &LEDController{
		logger: logger,
	}
}

func (l *LEDController) On() error {
	l.execScript(greenLedScript, "1")
	return nil
}

func (l *LEDController) Off() error {
	l.execScript(greenLedScript, "0")
	return nil
}

func (l *LEDController) Flash(duration time.Duration) {
	_ = l.On()
	time.AfterFunc(duration, func() {
		_ = l.Off()
	})
}

func (l *LEDController) Close() error {
	l.StopBlink()
	_ = l.Off()
	return nil
}

func (l *LEDController) StartBlink(interval time.Duration) {
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
				_ = l.Off()
				return
			case <-ticker.C:
				if state {
					_ = l.Off()
				} else {
					_ = l.On()
				}
				state = !state
			}
		}
	}()
}

func (l *LEDController) StopBlink() {
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

func (l *LEDController) Red() error {
	l.execScript(greenLedScript, "red")
	return nil
}

func (l *LEDController) Green() error {
	l.execScript(greenLedScript, "green")
	return nil
}

func (l *LEDController) Amber() error {
	l.execScript(greenLedScript, "amber")
	return nil
}

func (l *LEDController) execScript(script string, args ...string) {
	cmd := exec.Command(script, args...)
	if err := cmd.Run(); err != nil {
		l.logger.Warn("LED script failed", "script", script, "args", args, "error", err)
	}
}
