package keycard

import (
	"context"
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

// WatchCommands listens for management commands on a Redis list.
// Commands:
//   - "list"               — respond with all authorized UIDs
//   - "add:<uid>"          — authorize a new card
//   - "remove:<uid>"       — revoke a card
//   - "count"              — respond with number of authorized cards
//   - "set-master:<uid>"   — set master UID (use NONE to disable physical master),
//                            wipes authorized cards when uid != NONE
//   - "learn:start"        — enter learn mode (as if master card was tapped)
//   - "learn:stop"         — exit learn mode, saving any learned cards
//   - "learn:master:start" — enter master teach-in mode; next non-registered tap
//                            is appended as an additional master (multi-master)
//   - "learn:master:stop"  — exit master teach-in mode without committing
//   - "reset"              — wipe master + authorized lists, cancel any active
//                            mode, leave service idle (start-over from installer)
//
// Per-tap events during learn:master are published on the keycard:events
// PUBSUB channel — see redis.go (PublishKeycardEvent).
func (s *Service) WatchCommands(ctx context.Context) {
	s.logger.Info("Starting keycard command watcher", "key", keycardCommandList)

	handler := ipc.HandleRequests(s.redis.client, keycardCommandList, func(command string) error {
		s.logger.Info("Received keycard command", "command", command)

		switch {
		case command == "list":
			uids := s.auth.ListAuthorized()
			s.publishResult(fmt.Sprintf("count:%d", len(uids)))
			for _, uid := range uids {
				time.Sleep(listWriteSpacing)
				s.publishResult(fmt.Sprintf("card:%s", uid))
			}

		case command == "count":
			s.publishResult(fmt.Sprintf("count:%d", s.auth.GetAuthorizedCount()))

		case strings.HasPrefix(command, "add:"):
			uid := strings.TrimPrefix(command, "add:")
			uid = strings.TrimSpace(uid)
			if uid == "" {
				s.publishResult("error:empty uid")
				return nil
			}
			added, err := s.auth.AddAuthorized(uid)
			if err != nil {
				s.logger.Error("Failed to add authorized card", "uid", uid, "error", err)
				s.publishResult(fmt.Sprintf("error:%v", err))
				return nil
			}
			if added {
				s.logger.Info("Card authorized via command", "uid", uid)
				s.publishKeycardCounts()
				s.publishResult("ok")
			} else {
				s.publishResult("error:already authorized")
			}

		case strings.HasPrefix(command, "remove:"):
			uid := strings.TrimPrefix(command, "remove:")
			uid = strings.TrimSpace(uid)
			if uid == "" {
				s.publishResult("error:empty uid")
				return nil
			}
			removed, err := s.auth.RemoveAuthorized(uid)
			if err != nil {
				s.logger.Error("Failed to remove authorized card", "uid", uid, "error", err)
				s.publishResult(fmt.Sprintf("error:%v", err))
				return nil
			}
			if removed {
				s.logger.Info("Card revoked via command", "uid", uid)
				s.publishKeycardCounts()
				s.publishResult("ok")
			} else {
				s.publishResult("error:not found")
			}

		case strings.HasPrefix(command, "set-master:"):
			uid := strings.TrimPrefix(command, "set-master:")
			uid = strings.TrimSpace(uid)
			if uid == "" {
				s.publishResult("error:empty uid")
				return nil
			}
			if err := s.auth.SetMaster(strings.ToUpper(uid)); err != nil {
				s.logger.Error("Failed to set master", "uid", uid, "error", err)
				s.publishResult(fmt.Sprintf("error:%v", err))
			} else {
				s.masterLearningMode = false
				s.rgbLed.StopBlink()
				s.publishKeycardCounts()
				s.logger.Info("Master set via command", "uid", uid)
				s.publishResult("ok")
			}

		case command == "learn:start":
			if s.learnMode {
				s.publishResult("error:already in learn mode")
			} else if s.masterLearningMode {
				s.publishResult("error:in master learning mode")
			} else {
				s.enterLearnMode()
				s.logger.Info("Learn mode started via command")
				s.publishResult("ok")
			}

		case command == "learn:stop":
			if !s.learnMode {
				s.publishResult("error:not in learn mode")
			} else {
				s.exitLearnMode()
				s.logger.Info("Learn mode stopped via command")
				s.publishResult("ok")
			}

		case command == "learn:master:start":
			if s.masterTeachInMode {
				s.publishResult("error:already in master teach-in")
			} else if s.learnMode {
				s.publishResult("error:in learn mode")
			} else if s.masterLearningMode {
				s.publishResult("error:in master learning mode")
			} else {
				s.enterMasterTeachIn()
				s.logger.Info("Master teach-in started via command")
				s.publishResult("ok")
			}

		case command == "learn:master:stop":
			if !s.masterTeachInMode {
				s.publishResult("error:not in master teach-in")
			} else {
				s.exitMasterTeachIn()
				s.logger.Info("Master teach-in stopped via command")
				s.publishResult("ok")
			}

		case command == "reset":
			s.resetAll()
			s.logger.Info("Auth state reset via command")
			s.publishResult("ok")

		default:
			s.logger.Warn("Unknown keycard command", "command", command)
			s.publishResult("error:unknown command")
		}

		return nil
	})
	defer handler.Stop()

	<-ctx.Done()
	s.logger.Info("Stopping keycard command watcher")
}

// publishResult writes a result to the keycard hash for bluetooth-service to pick up.
//
// Sync(): forces HSET+PUBLISH to complete before returning. Without it the
// default fire-and-forget goroutines would race amongst themselves for
// multi-write sequences (see listWriteSpacing for why ordering matters).
func (s *Service) publishResult(result string) {
	if err := s.redis.client.Hash(keycardHashKey).Set("command-result", result, ipc.Sync()); err != nil {
		s.logger.Error("Failed to publish keycard result", "result", result, "error", err)
	}
}
