package keycard

import (
	"fmt"
	"log/slog"
	"time"

	hal "github.com/librescoot/pn7150"
)

// nfcReader is the subset of the PN7150 HAL used by Service. Keeping it
// narrow lets the service recover from an unavailable device and test that
// recovery without NFC hardware.
type nfcReader interface {
	Initialize() error
	StartDiscovery(uint) error
	StopDiscovery() error
	FullReinitialize() error
	AwaitReadable(time.Duration) error
	DetectTags() ([]hal.Tag, error)
	SelectTag(uint) error
	Close()
}

type nfcFactory func(*Config, *slog.Logger) (nfcReader, error)

func newPN7150NFC(config *Config, logger *slog.Logger) (nfcReader, error) {
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

	nfc, err := hal.NewPN7150(config.Device, logCallback, nil, true, false, config.Debug)
	if err != nil {
		return nil, fmt.Errorf("create NFC HAL: %w", err)
	}
	return nfc, nil
}
