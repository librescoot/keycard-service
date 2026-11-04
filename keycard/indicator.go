package keycard

import "time"

const (
	alarmPulseOn  = 100 * time.Millisecond
	alarmPulseOff = 1900 * time.Millisecond
)

var alarmPulseColor = LEDColor{Red: 80}

type indicatorState struct {
	closed             bool
	alarm              bool
	mode               string
	fault              bool
	foreground         bool
	blink              time.Duration
	feedbackTimer      *time.Timer
	feedbackGeneration uint64
	patternTimer       *time.Timer
	patternGeneration  uint64
}

func (l *LP5562) cancelFeedbackLocked() {
	l.indicator.feedbackGeneration++
	if l.indicator.feedbackTimer != nil {
		l.indicator.feedbackTimer.Stop()
		l.indicator.feedbackTimer = nil
	}
}
func (l *LP5562) cancelPatternLocked() {
	l.indicator.patternGeneration++
	if l.indicator.patternTimer != nil {
		l.indicator.patternTimer.Stop()
		l.indicator.patternTimer = nil
	}
}

func (l *LP5562) renderLocked() error {
	l.cancelPatternLocked()
	if l.indicator.closed {
		return nil
	}
	s := &l.indicator
	if s.foreground {
		if s.blink > 0 {
			return l.patternStepLocked(l.color, s.blink, s.blink, true, s.patternGeneration)
		}
		return l.applyColorLocked(l.color)
	}
	if s.fault {
		return l.patternStepLocked(ColorRed, blinkInterval, blinkInterval, true, s.patternGeneration)
	}
	switch s.mode {
	case "learn":
		return l.applyColorLocked(ColorAmber)
	case "master-bootstrap", "master-teach-in":
		return l.patternStepLocked(ColorAmber, blinkInterval, blinkInterval, true, s.patternGeneration)
	}
	if s.alarm {
		return l.patternStepLocked(alarmPulseColor, alarmPulseOn, alarmPulseOff, true, s.patternGeneration)
	}
	return l.applyColorLocked(ColorOff)
}

// Timer callbacks hold the same mutex as foreground changes. A callback that
// raced Stop cannot write an old frame over newer card feedback.
func (l *LP5562) patternStepLocked(color LEDColor, on, off time.Duration, lit bool, generation uint64) error {
	frame, delay := ColorOff, off
	if lit {
		frame, delay = color, on
	}
	err := l.applyColorLocked(frame)
	l.indicator.patternTimer = time.AfterFunc(delay, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.indicator.closed || generation != l.indicator.patternGeneration {
			return
		}
		l.logIfErr(l.patternStepLocked(color, on, off, !lit, generation))
	})
	return err
}

func (l *LP5562) SetColor(color LEDColor) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.closed {
		return nil
	}
	l.cancelFeedbackLocked()
	l.indicator.foreground = color != ColorOff
	l.indicator.blink = 0
	if color != ColorOff {
		l.color = color
	}
	return l.renderLocked()
}
func (l *LP5562) Off() error   { return l.SetColor(ColorOff) }
func (l *LP5562) Red() error   { return l.SetColor(ColorRed) }
func (l *LP5562) Green() error { return l.SetColor(ColorGreen) }
func (l *LP5562) Amber() error { return l.SetColor(ColorAmber) }
func (l *LP5562) On() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.closed {
		return nil
	}
	l.cancelFeedbackLocked()
	l.indicator.foreground = true
	return l.renderLocked()
}

func (l *LP5562) ReleaseAfter(duration time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.closed {
		return
	}
	l.releaseAfterLocked(duration)
}
func (l *LP5562) releaseAfterLocked(duration time.Duration) {
	l.cancelFeedbackLocked()
	generation := l.indicator.feedbackGeneration
	l.indicator.feedbackTimer = time.AfterFunc(duration, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.releaseFeedbackLocked(generation)
	})
}
func (l *LP5562) releaseFeedbackLocked(generation uint64) {
	if l.indicator.closed || generation != l.indicator.feedbackGeneration {
		return
	}
	l.indicator.feedbackTimer = nil
	l.indicator.foreground = false
	l.indicator.blink = 0
	l.logIfErr(l.renderLocked())
}
func (l *LP5562) Flash(duration time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.closed {
		return
	}
	l.color = ColorGreen
	l.indicator.foreground = true
	l.indicator.blink = 0
	l.logIfErr(l.renderLocked())
	l.releaseAfterLocked(duration)
}
func (l *LP5562) StartBlink(interval time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.closed || interval <= 0 {
		return
	}
	l.cancelFeedbackLocked()
	l.indicator.foreground = true
	l.indicator.blink = interval
	l.logIfErr(l.renderLocked())
}
func (l *LP5562) StopBlink() { l.logIfErr(l.Off()) }

func (l *LP5562) SetAlarmIndicator(enabled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.closed || l.indicator.alarm == enabled {
		return
	}
	l.indicator.alarm = enabled
	if !l.indicator.foreground && !l.indicator.fault && l.indicator.mode == "" {
		l.logIfErr(l.renderLocked())
	}
}
func (l *LP5562) SetFeedbackMode(mode string) {
	if mode == "idle" {
		mode = ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.closed || l.indicator.mode == mode {
		return
	}
	l.indicator.mode = mode
	if mode != "" {
		l.cancelFeedbackLocked()
		l.indicator.foreground = false
		l.indicator.blink = 0
	}
	l.logIfErr(l.renderLocked())
}
func (l *LP5562) SetNFCFault(active bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indicator.closed || l.indicator.fault == active {
		return
	}
	l.indicator.fault = active
	l.cancelFeedbackLocked()
	l.indicator.foreground = false
	l.indicator.blink = 0
	l.logIfErr(l.renderLocked())
}
