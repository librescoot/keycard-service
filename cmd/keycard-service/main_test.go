package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"
)

type lifecycleStub struct {
	run  func() error
	stop func()
}

func (s lifecycleStub) Run() error { return s.run() }
func (s lifecycleStub) Stop()      { s.stop() }

func TestSignalWaitsForCleanupAfterRunReturns(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	runReturned := make(chan struct{})
	finishCleanup := make(chan struct{})
	service := lifecycleStub{
		run:  func() error { close(started); <-cancelled; close(runReturned); return nil },
		stop: func() { close(cancelled); <-finishCleanup },
	}
	signals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { result <- runService(service, signals, logger) }()
	<-started
	signals <- syscall.SIGTERM
	<-runReturned
	select {
	case <-result:
		t.Fatal("process returned before cleanup")
	case <-time.After(20 * time.Millisecond):
	}
	close(finishCleanup)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
}

func TestUnexpectedRunExitAlsoCleansUp(t *testing.T) {
	cause := errors.New("run failure")
	stopped := false
	service := lifecycleStub{run: func() error { return cause }, stop: func() { stopped = true }}
	err := runService(service, make(chan os.Signal), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, cause) || !stopped {
		t.Fatalf("error=%v cleanup=%v", err, stopped)
	}
}
