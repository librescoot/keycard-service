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
	LEDDevice   string // I2C device for LP5662, empty for shell scripts
	LEDAddress  uint8  // I2C address for LP5662
}

type Service struct {
	config *Config
	logger *slog.Logger

	nfc       *hal.PN7150
	auth      *AuthManager
	rgbLed    RGBLed         // RGB LED for feedback (LP5662 or script-based)
	linearLed *LEDController // Linear LEDs for learn mode indicators
	redis     *RedisClient

	masterLearningMode bool
	learnMode          bool
	newUIDs            []string

	// Card presence tracking
	currentCardUID string    // UID of currently present card ("" if none)
	lastSeenTime   time.Time // Last time current card was detected
	emptyPollCount int       // Consecutive polls with no card detected

	ctx    context.Context
	cancel context.CancelFunc
}

func NewService(config *Config, logger *slog.Logger) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Service{
		config:         config,
		logger:         logger,
		ctx:            ctx,
		cancel:         cancel,
		currentCardUID: "",
		emptyPollCount: 0,
	}

	var err error

	s.auth, err = NewAuthManager(config.DataDir)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create auth manager: %w", err)
	}

	// Initialize LED controllers
	s.linearLed = NewLEDController(logger)

	if config.LEDDevice != "" {
		// Use LP5662 RGB LED driver
		lp5662, err := NewLP5662(config.LEDDevice, config.LEDAddress, logger)
		if err != nil {
			logger.Warn("Failed to initialize LP5662, falling back to script-based LED", "error", err)
			s.rgbLed = s.linearLed
		} else {
			s.rgbLed = lp5662
		}
	} else {
		// Use script-based LED control
		s.rgbLed = s.linearLed
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
	s.logger.Info("Keycard service starting",
		"device", s.config.Device,
		"dataDir", s.config.DataDir,
		"hasMaster", s.auth.HasMaster())

	if !s.auth.HasMaster() {
		s.enterMasterLearningMode()
	}

	// Enable event-driven detection
	s.nfc.SetTagEventReaderEnabled(true)
	defer s.nfc.SetTagEventReaderEnabled(false)

	// Start continuous discovery with short period
	if err := s.nfc.StartDiscovery(100); err != nil {
		if strings.Contains(err.Error(), "status: 06") {
			s.logger.Warn("Discovery failed with semantic error, reinitializing")
			if err := s.nfc.FullReinitialize(); err != nil {
				return fmt.Errorf("reinitialization failed: %w", err)
			}
			if err := s.nfc.StartDiscovery(100); err != nil {
				return fmt.Errorf("discovery failed after reinit: %w", err)
			}
		} else {
			return fmt.Errorf("failed to start discovery: %w", err)
		}
	}
	defer s.nfc.StopDiscovery()

	s.logger.Info("Event-driven tag detection enabled")

	// Event loop
	eventChan := s.nfc.GetTagEventChannel()
	for {
		select {
		case <-s.ctx.Done():
			s.logger.Info("Service shutting down")
			return nil
		case event, ok := <-eventChan:
			if !ok {
				s.logger.Error("Event channel closed unexpectedly")
				return fmt.Errorf("event channel closed")
			}
			if event.Error != nil {
				s.logger.Warn("Tag event error", "error", event.Error)
				continue
			}
			s.handleTagEvent(event)
		}
	}
}

func (s *Service) Stop() {
	s.cancel()
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

func (s *Service) handleTagEvent(event hal.TagEvent) {
	switch event.Type {
	case hal.TagArrival:
		uid := strings.ToUpper(hex.EncodeToString(event.Tag.ID))
		s.logger.Debug("Tag event: arrival", "uid", uid)
		s.handleTagDetection(uid)

	case hal.TagDeparture:
		s.logger.Debug("Tag event: departure")
		s.handleTagDeparture()
	}
}


func (s *Service) handleTagDetection(uid string) {
	// Check if this is a NEW card arrival
	s.logger.Debug("handleTagDetection", "detected_uid", uid, "current_uid", s.currentCardUID, "is_new", s.currentCardUID != uid)
	if s.currentCardUID != uid {
		// Different card - this is a new arrival
		s.logger.Info("Tag arrived", "uid", uid)
		s.currentCardUID = uid
		s.lastSeenTime = time.Now()
		s.emptyPollCount = 0
		s.handleTagArrival(uid) // Trigger actual arrival logic
	} else {
		// Same card still present - just update tracking
		s.lastSeenTime = time.Now()
		s.emptyPollCount = 0
		s.logger.Debug("Tag still present", "uid", uid)
	}
}

func (s *Service) handleTagDeparture() {
	if s.currentCardUID != "" {
		s.logger.Info("Tag departed", "uid", s.currentCardUID)
		s.currentCardUID = ""
		s.emptyPollCount = 0
	}
}

func (s *Service) handleTagArrival(uid string) {
	// Set LED to amber during lookup
	s.rgbLed.Amber()

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
		return
	}

	s.masterLearningMode = false
	s.rgbLed.StopBlink()
	s.rgbLed.Flash(flashDuration)

	s.logger.Info("Master UID learned successfully", "uid", uid)
}

func (s *Service) enterLearnMode() {
	s.logger.Info("Entering learn mode - present cards to authorize")
	s.learnMode = true
	s.newUIDs = nil
	s.linearLed.LedLinearOn(Led3)
	s.linearLed.LedLinearOn(Led7)
}

func (s *Service) exitLearnMode() {
	s.logger.Info("Exiting learn mode",
		"newUIDs", len(s.newUIDs),
		"totalAuthorized", s.auth.GetAuthorizedCount())

	s.learnMode = false
	s.linearLed.LedLinearOff(Led3)
	s.linearLed.LedLinearOff(Led7)
	s.newUIDs = nil
}

func (s *Service) learnUID(uid string) {
	added, err := s.auth.AddAuthorized(uid)
	if err != nil {
		s.logger.Error("Failed to add authorized UID", "uid", uid, "error", err)
		return
	}

	if added {
		s.newUIDs = append(s.newUIDs, uid)
		s.rgbLed.Flash(flashDuration)
		s.logger.Info("UID authorized", "uid", uid)
	} else {
		s.logger.Info("UID already authorized", "uid", uid)
	}
}

func (s *Service) grantAccess(uid string) {
	s.logger.Info("Access granted", "uid", uid)
	s.flashLED(s.rgbLed.Green, flashDuration)

	if err := s.redis.PublishAuth(uid); err != nil {
		s.logger.Error("Failed to publish auth to Redis", "error", err)
	}
}
