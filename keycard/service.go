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

// Triggers name what caused a state change, so a subscriber can tell a tap
// apart from a command.
const (
	TriggerCard      = "card"
	TriggerCommand   = "command"
	TriggerBootstrap = "bootstrap"
	TriggerTeachIn   = "teach-in"
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

	masterBootstrapMode bool
	masterTeachInMode   bool
	learnMode           bool
	newUIDs             []string

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

	if rejected := s.auth.Rejected(); len(rejected) > 0 {
		// Dropped at load, so gone from the files on the next write.
		s.logger.Warn("Ignored malformed UID file entries",
			"count", len(rejected), "entries", strings.Join(rejected, ", "))
	}

	s.publishKeycardCounts()
	if !s.auth.HasMaster() {
		// Only a factory-fresh reader bootstraps. With cards enrolled the next
		// tap is an owner expecting to unlock, and a master never unlocks; in
		// service mode a technician is driving the vehicle over commands.
		if s.auth.GetAuthorizedCount() > 0 {
			s.logger.Info("No master stored, but authorized cards exist - not entering master bootstrap")
		} else if s.redis.ServiceModeActive() {
			s.logger.Info("No master stored, but service mode is active - not entering master bootstrap")
		} else {
			s.enterMasterBootstrap()
		}
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
	if s.masterBootstrapMode || s.masterTeachInMode {
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

	if s.masterBootstrapMode {
		s.bootstrapMasterUID(uid)
		return
	}

	if !s.learnMode {
		if s.auth.IsMaster(uid) {
			s.enterLearnMode(TriggerCard)
		} else if s.auth.CanUnlock(uid) {
			s.grantAccess(uid)
		} else {
			s.logger.Info("Unauthorized UID", "uid", uid)
			s.flashLED(s.rgbLed.Red, flashDuration)
		}
	} else {
		if s.auth.IsMaster(uid) {
			s.exitLearnMode(TriggerCard)
		} else {
			s.learnUID(uid)
		}
	}
}

// enterMasterBootstrap arms the boot-time bootstrap: the next card presented
// becomes master. Announced, because no tap should claim the vehicle unobserved.
func (s *Service) enterMasterBootstrap() {
	s.logger.Info("Entering master bootstrap - the next card presented becomes master")
	s.masterBootstrapMode = true
	s.rgbLed.StartBlink(blinkInterval)
	s.publishEvent("mode-entered:master-bootstrap:boot")
}

// cancelMasterBootstrap leaves bootstrap without writing anything.
func (s *Service) cancelMasterBootstrap(trigger string) {
	if !s.masterBootstrapMode {
		return
	}
	s.masterBootstrapMode = false
	s.rgbLed.StopBlink()
	if err := s.rgbLed.Off(); err != nil {
		s.logger.Warn("Failed to set LED", "error", err)
	}
	s.logger.Info("Master bootstrap cancelled", "trigger", trigger)
	s.publishEvent("mode-exited:master-bootstrap:" + trigger)
}

func (s *Service) bootstrapMasterUID(uid string) {
	s.logger.Info("Learning master UID", "uid", uid)

	if err := s.auth.SetMaster(uid); err != nil {
		s.logger.Error("Failed to save master UID", "error", err)
		s.flashLED(s.rgbLed.Red, flashDuration)
		s.publishEvent("error:save-failed:" + uid)
		return
	}
	s.publishKeycardCounts()

	s.masterBootstrapMode = false
	s.rgbLed.StopBlink()
	s.rgbLed.Flash(flashDuration)

	s.logger.Info("Master UID learned successfully", "uid", uid)
	s.publishEvent("master-added:" + uid + ":" + TriggerBootstrap)
	s.publishEvent("mode-exited:master-bootstrap:" + TriggerCard)
}

// enterMasterTeachIn is the command-driven counterpart to bootstrap: it
// appends via AddMaster and rejects an already-registered UID.
func (s *Service) enterMasterTeachIn() {
	s.logger.Info("Entering master teach-in mode - present a fresh card to register as master")
	s.masterTeachInMode = true
	s.rgbLed.StartBlink(blinkInterval)
	s.publishEvent("mode-entered:master")
}

func (s *Service) exitMasterTeachIn() {
	s.masterTeachInMode = false
	s.rgbLed.StopBlink()
	s.publishEvent("mode-exited:master")
}

func (s *Service) teachInMasterUID(uid string) {
	uid = strings.ToUpper(uid)

	if s.auth.IsKnown(uid) {
		s.logger.Info("Master teach-in rejected: UID already registered", "uid", uid)
		s.flashLED(s.rgbLed.Red, flashDuration)
		s.publishEvent("rejected:already-authorized:" + uid)
		return
	}

	added, err := s.auth.AddMaster(uid)
	if err != nil {
		s.logger.Error("Failed to add master UID", "uid", uid, "error", err)
		s.flashLED(s.rgbLed.Red, flashDuration)
		s.publishEvent("error:save-failed:" + uid)
		return
	}
	if !added {
		// Race: AddMaster found the UID already present even though the
		// IsAuthorized check above missed it. Treat as a duplicate.
		s.flashLED(s.rgbLed.Red, flashDuration)
		s.publishEvent("rejected:already-authorized:" + uid)
		return
	}

	s.publishKeycardCounts()
	s.masterTeachInMode = false
	s.rgbLed.StopBlink()
	s.rgbLed.Flash(flashDuration)

	s.logger.Info("Master UID added via teach-in", "uid", uid)
	// master-learned is what the installer matches on; master-added is the
	// same fact in the vocabulary every other path uses.
	s.publishEvent("master-learned:" + uid)
	s.publishEvent("master-added:" + uid + ":" + TriggerTeachIn)
	s.publishEvent("mode-exited:master")
}

// resetAll wipes both lists and cancels any active mode, leaving the service
// idle rather than re-entering bootstrap; the next start decides that.
func (s *Service) resetAll() {
	if s.masterTeachInMode {
		s.exitMasterTeachIn()
	}
	s.cancelMasterBootstrap(TriggerCommand)
	if s.learnMode {
		s.learnMode = false
		s.blinkerLed.LedLinearOff(Led3)
		s.blinkerLed.LedLinearOff(Led7)
		s.newUIDs = nil
		if err := s.rgbLed.Off(); err != nil {
			s.logger.Warn("Failed to set LED", "error", err)
		}
		s.publishEvent("mode-exited:learn:" + TriggerCommand)
	}

	if err := s.auth.Reset(); err != nil {
		s.logger.Error("Failed to reset auth state", "error", err)
		s.flashLED(s.rgbLed.Red, flashDuration)
		return
	}

	s.publishKeycardCounts()
	s.logger.Info("Auth state reset")
	s.publishEvent("reset")
}

// Fire-and-forget: a failed event must not change what the vehicle does about
// the card in front of it.
func (s *Service) publishEvent(payload string) {
	if err := s.redis.PublishKeycardEvent(payload); err != nil {
		s.logger.Warn("Failed to publish event", "payload", payload, "error", err)
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

func (s *Service) enterLearnMode(trigger string) {
	s.logger.Info("Entering learn mode - present cards to authorize", "trigger", trigger)
	s.learnMode = true
	s.newUIDs = nil
	s.blinkerLed.LedLinearOn(Led3)
	s.blinkerLed.LedLinearOn(Led7)
	s.publishEvent("mode-entered:learn:" + trigger)
}

// exitLearnMode appends this session's UIDs to the authorized list.
func (s *Service) exitLearnMode(trigger string) {
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
	s.publishEvent("mode-exited:learn:" + trigger)
}

// learnUID queues a tap for exitLearnMode to commit. The per-tap event is the
// only live signal: the count hash does not move until the session persists.
func (s *Service) learnUID(uid string) {
	if s.auth.IsMaster(uid) {
		return
	}

	duplicate := s.auth.IsKnown(uid)
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
		s.publishEvent("card-duplicate:" + uid)
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
	s.publishEvent("card-learned:" + uid)
}

func (s *Service) grantAccess(uid string) {
	s.logger.Info("Access granted", "uid", uid)

	if err := s.redis.PublishAuth(uid); err != nil {
		s.logger.Error("Failed to publish auth to Redis", "error", err)
	}
	s.publishEvent("access-granted:" + uid)

	s.flashLED(s.rgbLed.Green, flashDuration)
}
