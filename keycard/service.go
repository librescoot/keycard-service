package keycard

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	blinkInterval = 500 * time.Millisecond
	flashDuration = 500 * time.Millisecond
)

type Config struct {
	Device     string
	DataDir    string
	RedisAddr  string
	Debug      bool
	LogLevel   int
	LEDDevice  string // LP5562 I2C device; empty selects the script fallback.
	LEDAddress uint8  // LP5562 I2C address.
}

type Service struct {
	config *Config
	logger *slog.Logger

	nfc              nfcReader
	nfcFactory       nfcFactory
	auth             *AuthManager
	rgbLed           RGBLed         // RAG card feedback LED.
	blinkerLed       *LEDController // Turn-signal LEDs indicate learn mode.
	redis            *RedisClient
	faults           nfcFaultReporter
	watchCommands    func(context.Context)
	waitForReconnect func(time.Duration) bool

	masterLearningMode bool
	masterTeachInMode  bool
	learnMode          bool
	newUIDs            []string

	currentCardUID string // Empty when no card is present.
	nfcFaultActive bool

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
		nfcFactory:     newPN7150NFC,
	}

	var err error

	s.auth, err = NewAuthManager(config.DataDir)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create auth manager: %w", err)
	}

	s.blinkerLed = NewLEDController(logger)

	if config.LEDDevice != "" {
		lp5562, err := NewLP5562(config.LEDDevice, config.LEDAddress, logger)
		if err != nil {
			logger.Warn("Failed to initialize LP5562, falling back to script-based LED", "error", err)
			s.rgbLed = s.blinkerLed
		} else {
			s.rgbLed = lp5562
		}
	} else {
		s.rgbLed = s.blinkerLed
	}

	s.redis, err = NewRedisClient(config.RedisAddr, logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create redis client: %w", err)
	}

	s.faults = s.redis
	s.watchCommands = s.WatchCommands
	s.waitForReconnect = s.waitForNFCReconnect
	return s, nil
}

type nfcFaultReporter interface {
	RaiseNFCUnavailableFault(description string) error
	ClearNFCUnavailableFault() error
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
	go s.watchCommands(s.ctx)

	backoff := time.Second
	for {
		if s.ctx.Err() != nil {
			return nil
		}

		if err := s.connectNFC(); err != nil {
			s.handleNFCFailure(err)
			if !s.waitForReconnect(backoff) {
				return nil
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		if err := s.startDiscovery(); err != nil {
			s.disconnectNFC()
			s.handleNFCFailure(err)
			if !s.waitForReconnect(backoff) {
				return nil
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		if err := s.faults.ClearNFCUnavailableFault(); err != nil {
			s.logger.Warn("Failed to clear NFC unavailable fault", "error", err)
		}
		s.clearNFCFaultIndication()
		backoff = time.Second
		s.logger.Info("NFC polling active")

		err := s.pollNFC()
		s.disconnectNFC()
		if s.ctx.Err() != nil {
			return nil
		}
		s.handleNFCFailure(err)
		if !s.waitForReconnect(backoff) {
			return nil
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Service) connectNFC() error {
	nfc, err := s.nfcFactory(s.config, s.logger)
	if err != nil {
		return fmt.Errorf("open NFC reader: %w", err)
	}
	if err := nfc.Initialize(); err != nil {
		nfc.Close()
		return fmt.Errorf("initialize NFC reader: %w", err)
	}
	s.nfc = nfc
	return nil
}

func (s *Service) startDiscovery() error {
	const pollPeriod = 200 // NFC discovery period, in milliseconds.

	if err := s.nfc.StartDiscovery(pollPeriod); err != nil {
		if !strings.Contains(err.Error(), "status: 06") {
			return fmt.Errorf("start discovery: %w", err)
		}
		s.logger.Warn("Discovery failed with semantic error, reinitializing")
		if err := s.nfc.FullReinitialize(); err != nil {
			return fmt.Errorf("reinitialize NFC reader: %w", err)
		}
		if err := s.nfc.StartDiscovery(pollPeriod); err != nil {
			return fmt.Errorf("start discovery after reinitialization: %w", err)
		}
	}
	return nil
}

func (s *Service) pollNFC() error {
	const (
		pollTimeout   = 5 * time.Second
		departureWait = 100 * time.Millisecond
	)

	for s.ctx.Err() == nil {
		if err := s.nfc.AwaitReadable(pollTimeout); err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			if err := s.startDiscovery(); err != nil {
				return err
			}
			continue
		}

		tags, err := s.nfc.DetectTags()
		if err != nil {
			s.logger.Debug("DetectTags error", "error", err)
			if err := s.startDiscovery(); err != nil {
				return err
			}
			continue
		}
		if len(tags) == 0 {
			continue
		}

		uid := strings.ToUpper(hex.EncodeToString(tags[0].ID))
		s.logger.Info("Tag arrived", "uid", uid)
		s.currentCardUID = uid
		s.handleTagArrival(uid)

		for {
			if s.ctx.Err() != nil {
				s.currentCardUID = ""
				return nil
			}
			time.Sleep(departureWait)
			if err := s.nfc.SelectTag(0); err != nil {
				break
			}
		}

		s.logger.Info("Tag departed", "uid", s.currentCardUID)
		s.currentCardUID = ""
		if err := s.startDiscovery(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) disconnectNFC() {
	if s.nfc == nil {
		return
	}
	if err := s.nfc.StopDiscovery(); err != nil {
		s.logger.Debug("Failed to stop NFC discovery", "error", err)
	}
	s.nfc.Close()
	s.nfc = nil
	s.currentCardUID = ""
}

func (s *Service) handleNFCFailure(err error) {
	if err == nil {
		err = fmt.Errorf("NFC polling stopped unexpectedly")
	}
	s.logger.Warn("NFC reader unavailable; will retry", "error", err)
	if raiseErr := s.faults.RaiseNFCUnavailableFault(err.Error()); raiseErr != nil {
		s.logger.Warn("Failed to raise NFC unavailable fault", "error", raiseErr)
	}
	if s.nfcFaultActive {
		return
	}
	s.nfcFaultActive = true
	if err := s.rgbLed.Red(); err != nil {
		s.logger.Warn("Failed to set NFC fault LED", "error", err)
	}
	s.rgbLed.StartBlink(blinkInterval)
}

func (s *Service) clearNFCFaultIndication() {
	if !s.nfcFaultActive {
		return
	}
	s.nfcFaultActive = false
	s.rgbLed.StopBlink()
	if s.masterLearningMode || s.masterTeachInMode {
		if err := s.rgbLed.Amber(); err != nil {
			s.logger.Warn("Failed to restore NFC LED", "error", err)
		}
		s.rgbLed.StartBlink(blinkInterval)
	}
}

func (s *Service) waitForNFCReconnect(delay time.Duration) bool {
	s.logger.Info("Waiting to retry NFC reader", "delay", delay)
	select {
	case <-s.ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

func (s *Service) Stop() {
	s.cancel()

	// Run owns the NFC handle and closes it before signalling done. Waiting
	// avoids racing an in-flight I2C operation during shutdown.
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		s.logger.Warn("Stop: timed out waiting for Run() to return")
	}

	if s.rgbLed != nil {
		if err := s.rgbLed.Close(); err != nil {
			s.logger.Warn("Failed to close LED", "error", err)
		}
	}
	if s.redis != nil {
		if err := s.redis.Close(); err != nil {
			s.logger.Warn("Failed to close redis client", "error", err)
		}
	}
}

func (s *Service) flashLED(setColor func() error, duration time.Duration) {
	if err := setColor(); err != nil {
		s.logger.Warn("Failed to set LED", "error", err)
	}
	time.AfterFunc(duration, func() {
		if err := s.rgbLed.Off(); err != nil {
			s.logger.Warn("Failed to set LED", "error", err)
		}
	})
}

func (s *Service) handleTagArrival(uid string) {
	if err := s.rgbLed.Amber(); err != nil {
		s.logger.Warn("Failed to set LED", "error", err)
	}

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
		s.learnMode = false
		s.blinkerLed.LedLinearOff(Led3)
		s.blinkerLed.LedLinearOff(Led7)
		s.newUIDs = nil
		if err := s.rgbLed.Off(); err != nil {
			s.logger.Warn("Failed to set LED", "error", err)
		}
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
	if s.redis == nil {
		return
	}
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

// exitLearnMode commits this session's collected UIDs to the authorized
// list. Cards are appended (not replaced) — to remove a card or wipe the
// list, use the remove:<uid> or reset commands. Any UID that has been
// concurrently authorized via another writer is silently skipped.
func (s *Service) exitLearnMode() {
	// handleTagArrival leaves the LED amber for the whole learn session, so
	// clear it here before any branch decides whether to flash. Without this
	// the sessions that neither flash nor error out (nothing presented, or
	// only already-authorized cards) leave the LED lit indefinitely.
	if err := s.rgbLed.Off(); err != nil {
		s.logger.Warn("Failed to set LED", "error", err)
	}

	if len(s.newUIDs) > 0 {
		added := 0
		var saveErr error
		for _, uid := range s.newUIDs {
			ok, err := s.auth.AddAuthorized(uid)
			if err != nil {
				saveErr = err
				s.logger.Error("Failed to save authorized UID", "uid", uid, "error", err)
				break
			}
			if ok {
				added++
			}
		}
		switch {
		case saveErr != nil:
			s.flashLED(s.rgbLed.Red, flashDuration)
		case added > 0:
			s.logger.Info("Authorized cards added",
				"added", added, "session", len(s.newUIDs))
			s.publishKeycardCounts()
			s.rgbLed.Flash(flashDuration)
		default:
			s.logger.Info("No new cards added (all already authorized)")
		}
	} else {
		s.logger.Info("No cards presented this session",
			"totalAuthorized", s.auth.GetAuthorizedCount())
	}

	s.learnMode = false
	s.blinkerLed.LedLinearOff(Led3)
	s.blinkerLed.LedLinearOff(Led7)
	s.newUIDs = nil
}

// learnUID is called for each non-master tap during learnMode. New UIDs are
// queued in s.newUIDs (committed by exitLearnMode); already-authorized or
// in-session UIDs are rejected with a red flash. Per-tap events are
// published to keycard:events so subscribers (e.g. the installer) can
// update live UI without waiting for the post-stop count refresh — the
// count hash itself stays stable until exitLearnMode persists the session.
func (s *Service) learnUID(uid string) {
	if s.auth.IsMaster(uid) {
		return
	}

	duplicate := s.auth.IsAuthorized(uid)
	if !duplicate {
		for _, existing := range s.newUIDs {
			if existing == uid {
				duplicate = true
				break
			}
		}
	}
	if duplicate {
		s.logger.Info("UID rejected as duplicate", "uid", uid)
		s.flashLED(s.rgbLed.Red, flashDuration)
		if err := s.redis.PublishKeycardEvent("card-duplicate:" + uid); err != nil {
			s.logger.Warn("Failed to publish event", "error", err)
		}
		return
	}

	s.newUIDs = append(s.newUIDs, uid)
	if err := s.rgbLed.Green(); err != nil {
		s.logger.Warn("Failed to set LED", "error", err)
	}
	time.AfterFunc(flashDuration, func() {
		if err := s.rgbLed.Amber(); err != nil {
			s.logger.Warn("Failed to set LED", "error", err)
		}
	})
	s.logger.Info("UID learned", "uid", uid)
	if err := s.redis.PublishKeycardEvent("card-learned:" + uid); err != nil {
		s.logger.Warn("Failed to publish event", "error", err)
	}
}

func (s *Service) grantAccess(uid string) {
	s.logger.Info("Access granted", "uid", uid)

	if err := s.redis.PublishAuth(uid); err != nil {
		s.logger.Error("Failed to publish auth to Redis", "error", err)
	}

	s.flashLED(s.rgbLed.Green, flashDuration)
}
