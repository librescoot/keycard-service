package keycard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

// listWriteSpacing is the gap between consecutive command-result writes for
// multi-response commands like "list". Reason: command-result is a single
// hash field; the bluetooth-service watcher does HGET on each pub/sub
// notification, and PUBLISH only carries the field name (not the value).
// Without spacing, back-to-back writes get coalesced — the reader's HGETs
// all return whichever value happened to be latest, so entries are missed
// and others are duplicated. 250 ms is comfortably above the BLE
// extended-response throttle (100 ms) and the typical pub/sub round trip,
// so each write has time to be observed before the next one lands.
const listWriteSpacing = 250 * time.Millisecond

const keycardCommandList = "scooter:keycard"

// command-result carries prose that existing clients compare verbatim;
// command-error carries the same outcome as a code, empty on success.
const (
	resultOK = "ok"

	codeEmptyUID          = "empty-uid"
	codeBadUID            = "bad-uid"
	codeAlreadyAuthorized = "already-authorized"
	codeAlreadyRegistered = "already-registered"
	codeNotFound          = "not-found"
	codeLastCredential    = "last-credential"
	codeSaveFailed        = "save-failed"
	codeUnknownCommand    = "unknown-command"
	codeWrongModePrefix   = "wrong-mode:"
)

// Wording preserved exactly: clients match on it, and the installer detects an
// old service by matching error:unknown here.
var errorProse = map[string]string{
	codeEmptyUID:          "error:empty uid",
	codeBadUID:            "error:invalid uid",
	codeAlreadyAuthorized: "error:already authorized",
	codeAlreadyRegistered: "error:already registered as a master",
	codeNotFound:          "error:not found",
	codeLastCredential:    "error:cannot remove last authorized card",
	codeSaveFailed:        "error:save failed",
	codeUnknownCommand:    "error:unknown command",
}

func prose(code string) string {
	if p, ok := errorProse[code]; ok {
		return p
	}
	return "error:" + code
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrEmptyUID):
		return codeEmptyUID
	case errors.Is(err, ErrInvalidUID):
		return codeBadUID
	case errors.Is(err, ErrLastCredential):
		return codeLastCredential
	default:
		return codeSaveFailed
	}
}

func (s *Service) currentMode() string {
	switch {
	case s.masterTeachInMode:
		return "master-teach-in"
	case s.masterBootstrapMode:
		return "master-bootstrap"
	case s.learnMode:
		return "learn"
	default:
		return "idle"
	}
}

// legacyModeProse reproduces the pre-command-error wording verbatim. Ordered to
// reproduce the old per-command answers, not to describe the mode tidily.
func legacyModeProse(command, mode string) string {
	switch {
	case command == "learn:stop":
		return "error:not in learn mode"
	case command == "learn:master:stop":
		return "error:not in master teach-in"
	case command == "learn:start" && mode == "learn":
		return "error:already in learn mode"
	case command == "learn:master:start" && mode == "master-teach-in":
		return "error:already in master teach-in"
	case mode == "master-bootstrap":
		return "error:in master learning mode"
	case mode == "learn":
		return "error:in learn mode"
	case mode == "master-teach-in":
		return "error:in master teach-in"
	default:
		return "error:wrong mode"
	}
}

// WatchCommands listens for management commands on a Redis list.
//
// Cards:
//   - "list"                    — respond with all authorized UIDs
//   - "count"                   — respond with the number of authorized cards
//   - "add:<uid>"               — authorize a card
//   - "remove:<uid>"            — revoke a card; refused with
//     error:last-credential when it is the only
//     card left that can unlock the vehicle
//
// Masters. A master card starts learn mode; it does not unlock the vehicle.
//   - "master:list"             — respond with the master UIDs
//   - "master:add:<uid>"        — append a master
//   - "master:remove:<uid>"     — drop a master
//   - "master:clear"            — empty the master list, keeping cards; the
//     next start re-arms boot-time bootstrap
//   - "master:bootstrap-cancel" — leave boot-time bootstrap without writing
//     anything
//   - "set-master:<uid>"        — replace the master list with one entry, or
//     with NONE for no physical master
//
// Modes:
//   - "learn:start" / "learn:stop"               — regular learn mode, as if
//     the master card had been tapped. Session taps are appended on stop.
//   - "learn:master:start" / "learn:master:stop" — master teach-in; the next
//     unregistered tap is appended as an additional master.
//   - "reset"                                    — wipe both lists, cancel any
//     active mode, leave the service idle.
//
// Every state change is also published on keycard:events, whether a command or
// a tap caused it.
func (s *Service) WatchCommands(ctx context.Context) {
	s.logger.Info("Starting keycard command watcher", "key", keycardCommandList)

	handler := ipc.HandleRequests(s.redis.client, keycardCommandList, func(command string) error {
		s.logger.Info("Received keycard command", "command", command)

		switch {
		case command == "list":
			s.respondList("card", s.auth.ListAuthorized())

		case command == "count":
			s.publishResult(fmt.Sprintf("count:%d", s.auth.GetAuthorizedCount()))

		case command == "master:list":
			s.respondList("master", s.auth.ListMasters())

		case strings.HasPrefix(command, "add:"):
			s.handleAdd(strings.TrimPrefix(command, "add:"))

		case strings.HasPrefix(command, "remove:"):
			s.handleRemove(strings.TrimPrefix(command, "remove:"))

		case strings.HasPrefix(command, "master:add:"):
			s.handleMasterAdd(strings.TrimPrefix(command, "master:add:"))

		case strings.HasPrefix(command, "master:remove:"):
			s.handleMasterRemove(strings.TrimPrefix(command, "master:remove:"))

		case command == "master:clear":
			if err := s.auth.ClearMasters(); err != nil {
				s.logger.Error("Failed to clear masters", "error", err)
				s.publishError(errorCode(err))
				return nil
			}
			s.publishKeycardCounts()
			s.publishEvent("masters-cleared")
			s.logger.Info("Master list cleared via command")
			s.publishResult(resultOK)

		case command == "master:bootstrap-cancel":
			// Idempotent: on a vehicle never in bootstrap this already holds.
			s.cancelMasterBootstrap(TriggerCommand)
			s.publishResult(resultOK)

		case strings.HasPrefix(command, "set-master:"):
			s.handleSetMaster(strings.TrimPrefix(command, "set-master:"))

		case command == "learn:start":
			if s.learnMode || s.masterTeachInMode {
				s.publishModeError(command)
			} else {
				// A command supersedes the bootstrap: it has answered the
				// question the bootstrap was waiting on.
				s.cancelMasterBootstrap(TriggerCommand)
				s.enterLearnMode(TriggerCommand)
				s.logger.Info("Learn mode started via command")
				s.publishResult(resultOK)
			}

		case command == "learn:stop":
			if !s.learnMode {
				s.publishModeError(command)
			} else {
				s.exitLearnMode(TriggerCommand)
				s.logger.Info("Learn mode stopped via command")
				s.publishResult(resultOK)
			}

		case command == "learn:master:start":
			if s.masterTeachInMode || s.learnMode {
				s.publishModeError(command)
			} else {
				s.cancelMasterBootstrap(TriggerCommand)
				s.enterMasterTeachIn()
				s.logger.Info("Master teach-in started via command")
				s.publishResult(resultOK)
			}

		case command == "learn:master:stop":
			// The installer sends this blind after a service start to make
			// sure no master-capturing mode is live, so it ends the bootstrap
			// too. master:bootstrap-cancel is the explicit spelling.
			switch {
			case s.masterTeachInMode:
				s.exitMasterTeachIn()
				s.logger.Info("Master teach-in stopped via command")
				s.publishResult(resultOK)
			case s.masterBootstrapMode:
				s.cancelMasterBootstrap(TriggerCommand)
				s.publishResult(resultOK)
			default:
				s.publishModeError(command)
			}

		case command == "reset":
			s.resetAll()
			s.logger.Info("Auth state reset via command")
			s.publishResult(resultOK)

		default:
			s.logger.Warn("Unknown keycard command", "command", command)
			s.publishError(codeUnknownCommand)
		}

		return nil
	})
	defer handler.Stop()

	<-ctx.Done()
	s.logger.Info("Stopping keycard command watcher")
}

func (s *Service) respondList(kind string, uids []string) {
	s.publishResult(fmt.Sprintf("count:%d", len(uids)))
	for _, uid := range uids {
		time.Sleep(listWriteSpacing)
		s.publishResult(kind + ":" + uid)
	}
}

func (s *Service) handleAdd(uid string) {
	added, err := s.auth.AddAuthorized(uid)
	if err != nil {
		s.logger.Error("Failed to add authorized card", "uid", uid, "error", err)
		s.publishError(errorCode(err))
		return
	}
	if !added {
		if s.auth.IsMaster(uid) {
			s.publishError(codeAlreadyRegistered)
		} else {
			s.publishError(codeAlreadyAuthorized)
		}
		return
	}

	normalized, _ := NormalizeUID(uid)
	s.logger.Info("Card authorized via command", "uid", normalized)
	s.publishKeycardCounts()
	s.publishEvent("card-added:" + normalized + ":" + TriggerCommand)
	s.publishResult(resultOK)
}

func (s *Service) handleRemove(uid string) {
	removed, err := s.auth.RemoveAuthorized(uid)
	if err != nil {
		s.logger.Error("Failed to remove authorized card", "uid", uid, "error", err)
		s.publishError(errorCode(err))
		return
	}
	if !removed {
		s.publishError(codeNotFound)
		return
	}

	normalized, _ := NormalizeUID(uid)
	s.logger.Info("Card revoked via command", "uid", normalized)
	s.publishKeycardCounts()
	s.publishEvent("card-removed:" + normalized + ":" + TriggerCommand)
	s.publishResult(resultOK)
}

func (s *Service) handleMasterAdd(uid string) {
	added, err := s.auth.AddMaster(uid)
	if err != nil {
		s.logger.Error("Failed to add master", "uid", uid, "error", err)
		s.publishError(errorCode(err))
		return
	}
	if !added {
		s.publishError(codeAlreadyRegistered)
		return
	}

	normalized, _ := NormalizeUID(uid)
	s.cancelMasterBootstrap(TriggerCommand)
	s.logger.Info("Master added via command", "uid", normalized)
	s.publishKeycardCounts()
	s.publishEvent("master-added:" + normalized + ":" + TriggerCommand)
	s.publishResult(resultOK)
}

func (s *Service) handleMasterRemove(uid string) {
	removed, err := s.auth.RemoveMaster(uid)
	if err != nil {
		s.logger.Error("Failed to remove master", "uid", uid, "error", err)
		s.publishError(errorCode(err))
		return
	}
	if !removed {
		s.publishError(codeNotFound)
		return
	}

	normalized, _ := NormalizeUID(uid)
	s.logger.Info("Master removed via command", "uid", normalized)
	s.publishKeycardCounts()
	s.publishEvent("master-removed:" + normalized + ":" + TriggerCommand)
	s.publishResult(resultOK)
}

// handleSetMaster replaces the master list, leaving authorized cards alone.
func (s *Service) handleSetMaster(uid string) {
	if err := s.auth.SetMaster(uid); err != nil {
		s.logger.Error("Failed to set master", "uid", uid, "error", err)
		s.publishError(errorCode(err))
		return
	}

	normalized, _ := NormalizeUID(uid)
	s.cancelMasterBootstrap(TriggerCommand)
	s.publishKeycardCounts()
	if normalized == MasterDisabled {
		s.publishEvent("masters-cleared")
	} else {
		s.publishEvent("master-added:" + normalized + ":" + TriggerCommand)
	}
	s.logger.Info("Master set via command", "uid", normalized)
	s.publishResult(resultOK)
}

// publishResult clears command-error so a stale code cannot be read as this
// command's.
func (s *Service) publishResult(result string) {
	s.publishAnswer(result, "")
}

func (s *Service) publishError(code string) {
	s.publishAnswer(prose(code), code)
}

func (s *Service) publishModeError(command string) {
	mode := s.currentMode()
	s.publishAnswer(legacyModeProse(command, mode), codeWrongModePrefix+mode)
}

// One notification, on command-result, so the pair is never seen half-updated.
// Sync(): see listWriteSpacing.
func (s *Service) publishAnswer(result, code string) {
	err := s.redis.client.Hash(keycardHashKey).SetManyPublishOne(map[string]any{
		"command-result": result,
		"command-error":  code,
	}, "command-result", ipc.Sync())
	if err != nil {
		s.logger.Error("Failed to publish keycard result",
			"result", result, "code", code, "error", err)
	}
}
