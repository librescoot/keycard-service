package keycard

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

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
			am.masterUIDs = append(am.masterUIDs, strings.ToUpper(uid))
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
			am.authorizedUIDs = append(am.authorizedUIDs, strings.ToUpper(uid))
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

func (am *AuthManager) GetAuthorizedCount() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return len(am.authorizedUIDs)
}

func (am *AuthManager) saveMasterUIDs() error {
	f, err := os.Create(am.masterFilePath())
	if err != nil {
		return err
	}
	defer f.Close()

	for _, uid := range am.masterUIDs {
		fmt.Fprintln(f, uid)
	}
	return nil
}

func (am *AuthManager) saveAuthorizedUIDs() error {
	f, err := os.Create(am.authorizedFilePath())
	if err != nil {
		return err
	}
	defer f.Close()

	for _, uid := range am.authorizedUIDs {
		fmt.Fprintln(f, uid)
	}
	return nil
}
