package keycard

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// atomicWriteLines writes lines to a file atomically using write-sync-rename pattern.
func atomicWriteLines(path string, lines []string) error {
	tmpPath := path + ".tmp"

	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	for _, line := range lines {
		if _, err := fmt.Fprintln(f, line); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return err
		}
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	f.Close()

	return os.Rename(tmpPath, path)
}

type AuthManager struct {
	mu             sync.RWMutex
	dataDir        string
	masterUIDs     []string
	authorizedUIDs []string
}

func NewAuthManager(dataDir string) (*AuthManager, error) {
	am := &AuthManager{
		dataDir: dataDir,
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	if err := am.loadMasterUIDs(); err != nil {
		return nil, fmt.Errorf("failed to load master UIDs: %w", err)
	}

	if err := am.loadAuthorizedUIDs(); err != nil {
		return nil, fmt.Errorf("failed to load authorized UIDs: %w", err)
	}

	return am, nil
}

func (am *AuthManager) masterFilePath() string {
	return filepath.Join(am.dataDir, "master_uids.txt")
}

func (am *AuthManager) authorizedFilePath() string {
	return filepath.Join(am.dataDir, "authorized_uids.txt")
}

func (am *AuthManager) loadMasterUIDs() error {
	am.masterUIDs = nil

	data, err := os.ReadFile(am.masterFilePath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		uid := strings.TrimSpace(scanner.Text())
		if uid != "" {
			// Normalize: remove spaces and uppercase
			uid = strings.ToUpper(strings.ReplaceAll(uid, " ", ""))
			am.masterUIDs = append(am.masterUIDs, uid)
		}
	}
	return scanner.Err()
}

func (am *AuthManager) loadAuthorizedUIDs() error {
	am.authorizedUIDs = nil

	data, err := os.ReadFile(am.authorizedFilePath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		uid := strings.TrimSpace(scanner.Text())
		if uid != "" {
			// Normalize: remove spaces and uppercase
			uid = strings.ToUpper(strings.ReplaceAll(uid, " ", ""))
			am.authorizedUIDs = append(am.authorizedUIDs, uid)
		}
	}
	return scanner.Err()
}

func (am *AuthManager) HasMaster() bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return len(am.masterUIDs) > 0
}

func (am *AuthManager) IsMaster(uid string) bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	uid = strings.ToUpper(uid)
	for _, m := range am.masterUIDs {
		if m == uid {
			return true
		}
	}
	return false
}

func (am *AuthManager) IsAuthorized(uid string) bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	uid = strings.ToUpper(uid)

	for _, m := range am.masterUIDs {
		if m == uid {
			return true
		}
	}

	for _, a := range am.authorizedUIDs {
		if a == uid {
			return true
		}
	}
	return false
}

func (am *AuthManager) SetMaster(uid string) error {
	am.mu.Lock()
	defer am.mu.Unlock()

	uid = strings.ToUpper(uid)
	am.masterUIDs = []string{uid}

	am.authorizedUIDs = nil

	if err := am.saveMasterUIDs(); err != nil {
		return err
	}
	return am.saveAuthorizedUIDs()
}

func (am *AuthManager) AddAuthorized(uid string) (bool, error) {
	am.mu.Lock()
	defer am.mu.Unlock()

	uid = strings.ToUpper(uid)

	for _, m := range am.masterUIDs {
		if m == uid {
			return false, nil
		}
	}

	for _, a := range am.authorizedUIDs {
		if a == uid {
			return false, nil
		}
	}

	am.authorizedUIDs = append(am.authorizedUIDs, uid)
	return true, am.saveAuthorizedUIDs()
}

func (am *AuthManager) RemoveAuthorized(uid string) (bool, error) {
	am.mu.Lock()
	defer am.mu.Unlock()

	uid = strings.ToUpper(uid)

	// Prevent removing the last authorized card (anti-lockout)
	if len(am.authorizedUIDs) <= 1 {
		return false, fmt.Errorf("cannot remove last authorized card")
	}

	for i, a := range am.authorizedUIDs {
		if a == uid {
			am.authorizedUIDs = append(am.authorizedUIDs[:i], am.authorizedUIDs[i+1:]...)
			return true, am.saveAuthorizedUIDs()
		}
	}

	return false, nil
}

func (am *AuthManager) ListAuthorized() []string {
	am.mu.RLock()
	defer am.mu.RUnlock()

	result := make([]string, len(am.authorizedUIDs))
	copy(result, am.authorizedUIDs)
	return result
}

func (am *AuthManager) ReplaceAuthorized(uids []string) error {
	am.mu.Lock()
	defer am.mu.Unlock()

	am.authorizedUIDs = uids
	return am.saveAuthorizedUIDs()
}

func (am *AuthManager) GetAuthorizedCount() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return len(am.authorizedUIDs)
}

func (am *AuthManager) saveMasterUIDs() error {
	return atomicWriteLines(am.masterFilePath(), am.masterUIDs)
}

func (am *AuthManager) saveAuthorizedUIDs() error {
	return atomicWriteLines(am.authorizedFilePath(), am.authorizedUIDs)
}
