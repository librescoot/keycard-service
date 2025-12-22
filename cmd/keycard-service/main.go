package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"keycard-service/keycard"
)

var version = "dev"

func main() {
	var (
		device     string
		dataDir    string
		redisAddr  string
		debug      bool
		logLevel   int
		ledDevice  string
		ledAddress uint
	)

	flag.StringVar(&device, "device", "/dev/pn5xx_i2c2", "NFC device path")
	flag.StringVar(&dataDir, "data-dir", "/data/keycard", "Data directory for UID files")
	flag.StringVar(&redisAddr, "redis", "localhost:6379", "Redis server address")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	flag.IntVar(&logLevel, "log", 2, "Log level (0=error, 1=warn, 2=info, 3=debug)")
	flag.StringVar(&ledDevice, "led-device", "", "I2C device for LP5662 RGB LED (empty for shell scripts)")
	flag.UintVar(&ledAddress, "led-address", 0x30, "I2C address for LP5662 RGB LED")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("keycard-service %s\n", version)
		return
	}

	var level slog.Level
	switch logLevel {
	case 0:
		level = slog.LevelError
	case 1:
		level = slog.LevelWarn
	case 2:
		level = slog.LevelInfo
	default:
		level = slog.LevelDebug
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))

	config := &keycard.Config{
		Device:     device,
		DataDir:    dataDir,
		RedisAddr:  redisAddr,
		Debug:      debug,
		LogLevel:   logLevel,
		LEDDevice:  ledDevice,
		LEDAddress: uint8(ledAddress),
	}

	service, err := keycard.NewService(config, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create service: %v\n", err)
		os.Exit(1)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		logger.Info("Received shutdown signal")
		service.Stop()
	}()

	ledInfo := "shell scripts"
	if ledDevice != "" {
		ledInfo = fmt.Sprintf("LP5662 at %s:0x%02X", ledDevice, ledAddress)
	}
	logger.Info(fmt.Sprintf("librescoot-keycard %s starting", version),
		"device", device,
		"dataDir", dataDir,
		"redis", redisAddr,
		"led", ledInfo)

	if err := service.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Service error: %v\n", err)
		os.Exit(1)
	}
}
