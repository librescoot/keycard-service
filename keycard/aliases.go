package keycard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// A 32-byte name encodes to 43 base64url characters. With a 32-hex-digit
// phone fingerprint, keycard:alias:set fits the 100-byte BLE command limit.
const maxAliasBytes = 32

var errInvalidAlias = errors.New("invalid key alias")

type keyAliases struct {
	mu    sync.RWMutex
	path  string
	names map[string]string
	fault error
}

func aliasKey(kind, id string) (string, error) {
	switch kind {
	case "card":
		uid, err := NormalizeUID(id)
		if err != nil || uid == MasterDisabled {
			return "", errInvalidAlias
		}
		return "card:" + uid, nil
	case "phone":
		id = strings.ToUpper(id)
		if len(id) != 32 {
			return "", errInvalidAlias
		}
		for _, c := range id {
			if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
				return "", errInvalidAlias
			}
		}
		return "phone:" + id, nil
	default:
		return "", errInvalidAlias
	}
}

func normalizeAliasName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxAliasBytes || !utf8.ValidString(name) {
		return "", errInvalidAlias
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return "", errInvalidAlias
		}
	}
	return name, nil
}

func newKeyAliases(dir string) (*keyAliases, error) {
	a := &keyAliases{path: filepath.Join(dir, "key_aliases.json"), names: make(map[string]string)}
	data, err := os.ReadFile(a.path)
	if os.IsNotExist(err) {
		return a, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &a.names)
	}
	if err == nil && a.names == nil {
		err = errInvalidAlias
	}
	if err == nil {
		for key, name := range a.names {
			parts := strings.SplitN(key, ":", 2)
			if len(parts) != 2 {
				err = errInvalidAlias
				break
			}
			canonical, keyErr := aliasKey(parts[0], parts[1])
			clean, nameErr := normalizeAliasName(name)
			if keyErr != nil || nameErr != nil || canonical != key || clean != name {
				err = errInvalidAlias
				break
			}
		}
	}
	if err != nil {
		a.names = make(map[string]string)
		a.fault = fmt.Errorf("load key aliases: %w", err)
		return a, a.fault
	}
	return a, nil
}

func (a *keyAliases) health() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.fault
}

func (a *keyAliases) list() map[string]string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := make(map[string]string, len(a.names))
	for key, name := range a.names {
		result[key] = name
	}
	return result
}

func (a *keyAliases) save() error {
	data, err := json.Marshal(a.names)
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, a.path); err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func (a *keyAliases) set(key, name string) error {
	name, err := normalizeAliasName(name)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fault != nil {
		return a.fault
	}
	old, existed := a.names[key]
	if existed && old == name {
		return nil
	}
	a.names[key] = name
	if err := a.save(); err != nil {
		if existed {
			a.names[key] = old
		} else {
			delete(a.names, key)
		}
		return err
	}
	return nil
}

func (a *keyAliases) clear(key string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fault != nil {
		return a.fault
	}
	old, exists := a.names[key]
	if !exists {
		return nil
	}
	delete(a.names, key)
	if err := a.save(); err != nil {
		a.names[key] = old
		return err
	}
	return nil
}

func (a *keyAliases) prune(valid map[string]bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fault != nil {
		return a.fault
	}
	removed := make(map[string]string)
	for key, name := range a.names {
		if !valid[key] {
			removed[key] = name
			delete(a.names, key)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := a.save(); err != nil {
		for key, name := range removed {
			a.names[key] = name
		}
		return err
	}
	return nil
}

func sortedAliases(names map[string]string) []string {
	keys := make([]string, 0, len(names))
	for key := range names {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
