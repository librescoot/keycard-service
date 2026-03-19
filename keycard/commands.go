package keycard

import (
	"context"
	"fmt"
	"strings"

	ipc "github.com/librescoot/redis-ipc"
)

const keycardCommandList = "scooter:keycard"

// WatchCommands listens for management commands on a Redis list.
// Commands:
//   - "list"              — respond with all authorized UIDs
//   - "add:<uid>"         — authorize a new card
//   - "remove:<uid>"      — revoke a card
//   - "count"             — respond with number of authorized cards
func (s *Service) WatchCommands(ctx context.Context) {
	s.logger.Info("Starting keycard command watcher", "key", keycardCommandList)

	handler := ipc.HandleRequests(s.redis.client, keycardCommandList, func(command string) error {
		s.logger.Info("Received keycard command", "command", command)

		switch {
		case command == "list":
			uids := s.auth.ListAuthorized()
			s.publishResult(fmt.Sprintf("count:%d", len(uids)))
			for _, uid := range uids {
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
				s.publishResult("ok")
			} else {
				s.publishResult("error:not found")
			}

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
func (s *Service) publishResult(result string) {
	if err := s.redis.client.Hash(keycardHashKey).Set("command-result", result); err != nil {
		s.logger.Error("Failed to publish keycard result", "result", result, "error", err)
	}
}
