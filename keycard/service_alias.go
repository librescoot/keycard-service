package keycard

import (
	"encoding/base64"
	"errors"
	"strings"
)

func (s *Service) validAliasKeys() map[string]bool {
	valid := make(map[string]bool)
	for _, uid := range s.auth.ListAuthorized() {
		valid["card:"+uid] = true
	}
	for _, uid := range s.auth.ListMasters() {
		if uid != MasterDisabled {
			valid["card:"+uid] = true
		}
	}
	if s.phones != nil && s.phones.health() == nil {
		for _, id := range s.phones.list() {
			valid["phone:"+id] = true
		}
	}
	return valid
}

func (s *Service) aliasSnapshot() map[string]string {
	if s.aliases == nil || s.aliases.health() != nil {
		return nil
	}
	valid := s.validAliasKeys()
	if s.phones == nil || s.phones.health() == nil {
		if err := s.aliases.prune(valid); err != nil {
			s.logger.Warn("Failed to prune key names", "error", err)
		}
	}
	names := s.aliases.list()
	for key := range names {
		if !valid[key] {
			delete(names, key)
		}
	}
	return names
}

func (s *Service) publishAliasSnapshot() {
	if s.redis == nil {
		return
	}
	if err := s.redis.PublishKeyAliases(s.aliasSnapshot()); err != nil {
		s.logger.Warn("Failed to publish key names", "error", err)
	}
}

func (s *Service) handleAliasList() {
	if s.aliases == nil || s.aliases.health() != nil {
		s.publishError(codeSaveFailed)
		return
	}
	names := s.aliasSnapshot()
	entries := make([]string, 0, len(names))
	for _, key := range sortedAliases(names) {
		entries = append(entries, key+":"+base64.RawURLEncoding.EncodeToString([]byte(names[key])))
	}
	s.respondList("alias", entries)
}

func (s *Service) handleAliasMutation(command string, clear bool) {
	if s.aliases == nil || s.aliases.health() != nil {
		s.publishError(codeSaveFailed)
		return
	}
	parts := strings.SplitN(command, ":", 3)
	if len(parts) < 2 || (!clear && len(parts) != 3) || (clear && len(parts) != 2) {
		s.publishError(codeInvalidAlias)
		return
	}
	key, err := aliasKey(parts[0], parts[1])
	if err != nil {
		s.publishError(codeInvalidAlias)
		return
	}
	if !s.validAliasKeys()[key] {
		s.publishError(codeNotFound)
		return
	}
	if clear {
		err = s.aliases.clear(key)
	} else {
		var decoded []byte
		decoded, err = base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			err = errInvalidAlias
		} else {
			err = s.aliases.set(key, string(decoded))
		}
	}
	if err != nil {
		if errors.Is(err, errInvalidAlias) {
			s.publishError(codeInvalidAlias)
		} else {
			s.logger.Error("Failed to save key name", "key", key, "error", err)
			s.publishError(codeSaveFailed)
		}
		return
	}
	s.publishAliasSnapshot()
	s.publishResult(resultOK)
}
