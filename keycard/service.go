package keycard

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	hal "github.com/librescoot/pn7150"
)

const (
	blinkInterval = 500 * time.Millisecond
	flashDuration = 500 * time.Millisecond
)

type Config struct {
	Device      string
	DataDir     string
	RedisAddr   string
	Debug       bool
	LogLevel    int
	LEDDevice   string // I2C device for LP5562, empty for shell scripts
	LEDAddress  uint8  // I2C address for LP5562
}

type Service struct {
	config *Config
	logger *slog.Logger

	nfc        *hal.PN7150
	auth       *AuthManager
	rgbLed     RGBLed         // RAG keycard LED for feedback (LP5562 or script-based)
	blinkerLed *LEDController // Turn signal blinkers (Led3, Led7), used as learn-mode indicator
	redis      *RedisClient

	masterLearningMode bool
	masterTeachInMode  bool
	learnMode          bool
	newUIDs            []string

	currentCardUID string // UID of currently present card ("" if none)

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func NewService(config *Config, logger *slog.Logger) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Service{
		config:         config,
		logger:         logger,
		ctx:            ctx,
		cancel:         cancel,
		currentCardUID: "",
		done:           make(chan struct{}),
	}

	var err error

	s.auth, err = NewAuthManager(config.DataDir)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create auth manager: %w", err)
	}

	// Initialize LED controllers
	s.blinkerLed = NewLEDController(logger)

	if config.LEDDevice != "" {
		// Use LP5562 LED driver
		lp5562, err := NewLP5562(config.LEDDevice, config.LEDAddress, logger)
		if err != nil {
			logger.Warn("Failed to initialize LP5562, falling back to script-based LED", "error", err)
			s.rgbLed = s.blinkerLed
		} else {
			s.rgbLed = lp5562
		}
	} else {
		// Use script-based LED control
		s.rgbLed = s.blinkerLed
	}

	s.redis, err = NewRedisClient(config.RedisAddr, logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create redis client: %w", err)
	}

	logCallback := func(level hal.LogLevel, message string) {
		if int(level) > config.LogLevel {
			return
		}
		switch level {
		case hal.LogLevelError:
			logger.Error(message)
		case hal.LogLevelWarning:
			logger.Warn(message)
		case hal.LogLevelInfo:
			logger.Info(message)
		case hal.LogLevelDebug:
			logger.Debug(message)
		}
	}

	s.nfc, err = hal.NewPN7150(config.Device, logCallback, nil, true, false, config.Debug)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create NFC HAL: %w", err)
	}

	if err := s.nfc.Initialize(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to initialize NFC HAL: %w", err)
	}

	return s, nil
}

func (s *Service) Run() error {
	defer close(s.done)

	s.logger.Info("Keycard service starting",
		"device", s.config.Device,
		"dataDir", s.config.DataDir,
		"hasMaster", s.auth.HasMaster())

	s.publishKeycardCounts()

	if !s.auth.HasMaster() {
		s.enterMasterLearningMode()
	}

	// Start Redis command listener in background
	go s.WatchCommands(s.ctx)

	const (
		pollPeriod    = 200 // ms — NFC chip discovery poll period
		pollTimeout   = 5 * time.Second
		departureWait = 100 * time.Millisecond
	)

	startDiscovery := func() error {
		if err := s.nfc.StartDiscovery(pollPeriod); err != nil {
			if strings.Contains(err.Error(), "status: 06") {
				s.logger.Warn("Discovery failed with semantic error, reinitializing")
				if err := s.nfc.FullReinitialize(); err != nil {
					return fmt.Errorf("reinitialization failed: %w", err)
				}
				if err := s.nfc.StartDiscovery(pollPeriod); err != nil {
					return fmt.Errorf("discovery failed after reinit: %w", err)
				}
			} else {
				return fmt.Errorf("failed to start discovery: %w", err)
			}
		}
		return nil
	}

	if err := startDiscovery(); err != nil {
		return err
	}
	defer s.nfc.StopDiscovery()

	s.logger.Info("NFC polling active")

	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
		}

		// Wait for NFC chip to report data
		if err := s.nfc.AwaitReadable(pollTimeout); err != nil {
			select {
			case <-s.ctx.Done():
				return nil
			default:
			}
			if err := startDiscovery(); err != nil {
				return err
			}
			continue
		}

		// Read the NCI notification
		tags, err := s.nfc.DetectTags()
		if err != nil {
			s.logger.Debug("DetectTags error", "error", err)
			select {
			case <-s.ctx.Done():
				return nil
			default:
			}
			if err := startDiscovery(); err != nil {
				return err
			}
			continue
		}
		if len(tags) == 0 {
			continue
		}

		// Tag detected
		uid := strings.ToUpper(hex.EncodeToString(tags[0].ID))
		s.logger.Info("Tag arrived", "uid", uid)
		s.currentCardUID = uid
		s.handleTagArrival(uid)

		// Wait for tag to depart using SLEEP→SELECT cycle
		departed := false
		for !departed {
			select {
			case <-s.ctx.Done():
				s.currentCardUID = ""
				return nil
			default:
			}
			time.Sleep(departureWait)
			if err := s.nfc.SelectTag(0); err != nil {
				departed = true
			}
		}

		s.logger.Info("Tag departed", "uid", s.currentCardUID)
		s.currentCardUID = ""

		select {
		case <-s.ctx.Done():
			return nil
		default:
		}
		if err := startDiscovery(); err != nil {
			return err
		}
	}
}

func (s *Service) Stop() {
	s.cancel()

	// Wait for Run() to return before tearing down the HAL, otherwise
	// an in-flight poll iteration can race past its ctx.Done() check
	// and issue an I2C op against the just-Deinitialized NFC chip.
	// Timeout safety net so a stuck Run() can't hang systemd shutdown.
	// If Run() was never entered (e.g. main aborts before calling it),
	// the timeout path keeps Stop() from blocking forever.
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		s.logger.Warn("Stop: timed out waiting for Run() to return, tearing down anyway")
	}

	if s.rgbLed != nil {
		s.rgbLed.Close()
	}
	if s.nfc != nil {
		s.nfc.Deinitialize()
	}
	if s.redis != nil {
		s.redis.Close()
	}
}

func (s *Service) flashLED(setColor func() error, duration time.Duration) {
	setColor()
	time.AfterFunc(duration, func() {
		s.rgbLed.Off()
	})
}

func (s *Service) handleTagArrival(uid string) {
	// Set LED to amber during lookup
	s.rgbLed.Amber()

	if s.masterTeachInMode {
		s.teachInMasterUID(uid)
		return
	}

	if s.masterLearningMode {
		s.learnMasterUID(uid)
		return
	}

	if !s.learnMode {
		if s.auth.IsMaster(uid) {
			s.enterLearnMode()
		} else if s.auth.IsAuthorized(uid) {
			s.grantAccess(uid)
		} else {
			s.logger.Info("Unauthorized UID", "uid", uid)
			s.flashLED(s.rgbLed.Red, flashDuration)
		}
	} else {
		if s.auth.IsMaster(uid) {
			s.exitLearnMode()
		} else {
			s.learnUID(uid)
		}
	}
}

func (s *Service) enterMasterLearningMode() {
	s.logger.Info("Entering master learning mode - present master card")
	s.masterLearningMode = true
	s.rgbLed.StartBlink(blinkInterval)
}

func (s *Service) learnMasterUID(uid string) {
	s.logger.Info("Learning master UID", "uid", uid)

	if err := s.auth.SetMaster(uid); err != nil {
		s.logger.Error("Failed to save master UID", "error", err)
		s.flashLED(s.rgbLed.Red, flashDuration)
		return
	}
	s.publishKeycardCounts()

	s.masterLearningMode = false
	s.rgbLed.StopBlink()
	s.rgbLed.Flash(flashDuration)

	s.logger.Info("Master UID learned successfully", "uid", uid)
}

// enterMasterTeachIn enters the command-driven master teach-in mode used by
// the installer flow. Unlike masterLearningMode (which fires automatically at
// boot when no master exists and uses SetMaster — wiping authorized cards),
// this mode appends a master via AddMaster and rejects taps that match an
// already-registered UID.
func (s *Service) enterMasterTeachIn() {
	s.logger.Info("Entering master teach-in mode - present a fresh card to register as master")
	s.masterTeachInMode = true
	s.rgbLed.StartBlink(blinkInterval)
	if err := s.redis.PublishKeycardEvent("mode-entered:master"); err != nil {
		s.logger.Warn("Failed to publish event", "error", err)
	}
}

func (s *Service) exitMasterTeachIn() {
	s.masterTeachInMode = false
	s.rgbLed.StopBlink()
	if err := s.redis.PublishKeycardEvent("mode-exited:master"); err != nil {
		s.logger.Warn("Failed to publish event", "error", err)
	}
}

func (s *Service) teachInMasterUID(uid string) {
	uid = strings.ToUpper(uid)

	if s.auth.IsAuthorized(uid) {
		s.logger.Info("Master teach-in rejected: UID already registered", "uid", uid)
		s.flashLED(s.rgbLed.Red, flashDuration)
		if err := s.redis.PublishKeycardEvent("rejected:already-authorized:" + uid); err != nil {
			s.logger.Warn("Failed to publish event", "error", err)
		}
		return
	}

	added, err := s.auth.AddMaster(uid)
	if err != nil {
		s.logger.Error("Failed to add master UID", "uid", uid, "error", err)
		s.flashLED(s.rgbLed.Red, flashDuration)
		if err := s.redis.PublishKeycardEvent("error:save-failed:" + uid); err != nil {
			s.logger.Warn("Failed to publish event", "error", err)
		}
		return
	}
	if !added {
		// Race: AddMaster found the UID already present even though the
		// IsAuthorized check above missed it. Treat as a duplicate.
		s.flashLED(s.rgbLed.Red, flashDuration)
		if err := s.redis.PublishKeycardEvent("rejected:already-authorized:" + uid); err != nil {
			s.logger.Warn("Failed to publish event", "error", err)
		}
		return
	}

	s.publishKeycardCounts()
	s.masterTeachInMode = false
	s.rgbLed.StopBlink()
	s.rgbLed.Flash(flashDuration)

	s.logger.Info("Master UID added via teach-in", "uid", uid)
	if err := s.redis.PublishKeycardEvent("master-learned:" + uid); err != nil {
		s.logger.Warn("Failed to publish event", "error", err)
	}
	if err := s.redis.PublishKeycardEvent("mode-exited:master"); err != nil {
		s.logger.Warn("Failed to publish event", "error", err)
	}
}

// resetAll cancels any active mode, wipes master + authorized lists, and
// republishes counts. Does not auto-enter masterLearningMode — leaves the
// service idle so the caller can drive the next state explicitly. On the
// next service restart, the boot-time HasMaster() check fires auto-learn
// as usual.
func (s *Service) resetAll() {
	if s.masterTeachInMode {
		s.exitMasterTeachIn()
	}
	if s.masterLearningMode {
		s.masterLearningMode = false
		s.rgbLed.StopBlink()
	}
	if s.learnMode {
		// Discard any cards collected this session.
		s.learnMode = false
		s.blinkerLed.LedLinearOff(Led3)
		s.blinkerLed.LedLinearOff(Led7)
		s.newUIDs = nil
	}

	if err := s.auth.Reset(); err != nil {
		s.logger.Error("Failed to reset auth state", "error", err)
		s.flashLED(s.rgbLed.Red, flashDuration)
		return
	}

	s.publishKeycardCounts()
	s.logger.Info("Auth state reset")
	if err := s.redis.PublishKeycardEvent("reset"); err != nil {
		s.logger.Warn("Failed to publish event", "error", err)
	}
}

func (s *Service) publishKeycardCounts() {
	if err := s.redis.PublishKeycardCounts(s.auth.GetMasterCount(), s.auth.GetAuthorizedCount()); err != nil {
		s.logger.Warn("Failed to publish keycard counts", "error", err)
	}
}

func (s *Service) enterLearnMode() {
	s.logger.Info("Entering learn mode - present cards to authorize")
	s.learnMode = true
	s.newUIDs = nil
	s.blinkerLed.LedLinearOn(Led3)
	s.blinkerLed.LedLinearOn(Led7)
}

func (s *Service) exitLearnMode() {
	if len(s.newUIDs) > 0 {
		// Replace all authorized cards with the ones learned this session
		if err := s.auth.ReplaceAuthorized(s.newUIDs); err != nil {
			s.logger.Error("Failed to save authorized UIDs", "error", err)
			s.flashLED(s.rgbLed.Red, flashDuration)
		} else {
			s.logger.Info("Authorized cards replaced",
				"newCards", len(s.newUIDs))
			s.publishKeycardCounts()
			s.rgbLed.Flash(flashDuration)
		}
	} else {
		s.logger.Info("No new cards learned, keeping existing cards",
			"totalAuthorized", s.auth.GetAuthorizedCount())
	}

	s.learnMode = false
	s.blinkerLed.LedLinearOff(Led3)
	s.blinkerLed.LedLinearOff(Led7)
	s.newUIDs = nil
}

func (s *Service) learnUID(uid string) {
	// Skip master cards
	if s.auth.IsMaster(uid) {
		return
	}

	// Deduplicate within current session
	for _, existing := range s.newUIDs {
		if existing == uid {
			s.logger.Info("UID already presented this session", "uid", uid)
			return
		}
	}

	s.newUIDs = append(s.newUIDs, uid)
	s.rgbLed.Green()
	time.AfterFunc(flashDuration, func() {
		s.rgbLed.Amber()
	})
	s.logger.Info("UID learned", "uid", uid)
}

func (s *Service) grantAccess(uid string) {
	s.logger.Info("Access granted", "uid", uid)

	if err := s.redis.PublishAuth(uid); err != nil {
		s.logger.Error("Failed to publish auth to Redis", "error", err)
	}

	s.flashLED(s.rgbLed.Green, flashDuration)
}
