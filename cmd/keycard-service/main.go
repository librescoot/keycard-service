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

func main() {
	var (
		device    string
		dataDir   string
		redisAddr string
		debug     bool
		logLevel  int
	)

	flag.StringVar(&device, "device", "/dev/pn5xx_i2c2", "NFC device path")
	flag.StringVar(&dataDir, "data-dir", "/data/keycard", "Data directory for UID files")
	flag.StringVar(&redisAddr, "redis", "localhost:6379", "Redis server address")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	flag.IntVar(&logLevel, "log", 2, "Log level (0=error, 1=warn, 2=info, 3=debug)")
	flag.Parse()

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
		Device:    device,
		DataDir:   dataDir,
		RedisAddr: redisAddr,
		Debug:     debug,
		LogLevel:  logLevel,
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

	logger.Info("Starting keycard service",
		"device", device,
		"dataDir", dataDir,
		"redis", redisAddr)

	if err := service.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Service error: %v\n", err)
		os.Exit(1)
	}
}
