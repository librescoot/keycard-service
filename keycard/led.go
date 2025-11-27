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

func (l *LEDController) GreenOn() {
	l.execScript(greenLedScript, "1")
}

func (l *LEDController) GreenOff() {
	l.execScript(greenLedScript, "0")
}

func (l *LEDController) GreenFlash(duration time.Duration) {
	l.GreenOn()
	time.AfterFunc(duration, func() {
		l.GreenOff()
	})
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
				l.GreenOff()
				return
			case <-ticker.C:
				if state {
					l.GreenOff()
				} else {
					l.GreenOn()
				}
				state = !state
			}
		}
	}()
}

func (l *LEDController) StopBlink() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.blinking {
		return
	}

	close(l.blinkStop)
	l.blinking = false
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
