package keycard

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// atomicWriteLines persists a complete UID list with write-sync-rename.
func atomicWriteLines(path string, lines []string) error {
	tmpPath := path + ".tmp"

	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer f.Close()

	for _, line := range lines {
		if _, err := fmt.Fprintln(f, line); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
	}

	if err := f.Sync(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, path)
}

type AuthManager struct {
	mu             sync.RWMutex
	dataDir        string
	masterUIDs     []string
	authorizedUIDs []string
	rejected       []string
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

// loadUIDFile drops blank, comment and malformed lines; a malformed entry can
// never match a tap.
func loadUIDFile(path string) ([]string, []string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	var uids, rejected []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		uid, err := NormalizeUID(line)
		if err != nil {
			rejected = append(rejected, line)
			continue
		}
		uids = append(uids, uid)
	}
	return uids, rejected, scanner.Err()
}

func (am *AuthManager) loadMasterUIDs() error {
	uids, rejected, err := loadUIDFile(am.masterFilePath())
	if err != nil {
		return err
	}
	am.masterUIDs = uids
	am.rejected = append(am.rejected, rejected...)
	return nil
}

func (am *AuthManager) loadAuthorizedUIDs() error {
	uids, rejected, err := loadUIDFile(am.authorizedFilePath())
	if err != nil {
		return err
	}

	// Masters load first and retain their role if manually edited files contain
	// a cross-role duplicate. This matches tap handling, where master behavior
	// takes precedence, while ensuring a UID is never active in both roles.
	am.authorizedUIDs = make([]string, 0, len(uids))
	removedConflict := false
	for _, uid := range uids {
		if uid == MasterDisabled || contains(am.masterUIDs, uid) {
			rejected = append(rejected, uid)
			removedConflict = true
			continue
		}
		am.authorizedUIDs = append(am.authorizedUIDs, uid)
	}
	am.rejected = append(am.rejected, rejected...)
	if removedConflict {
		return am.saveAuthorizedUIDs()
	}
	return nil
}

// Rejected returns malformed or conflicting lines dropped at load, for logging
// once at startup.
func (am *AuthManager) Rejected() []string {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return append([]string(nil), am.rejected...)
}

var (
	// ErrLastCredential means a removal would leave no card able to unlock.
	// Masters do not count towards that: a master never grants access.
	ErrLastCredential = errors.New("would remove the last card that can unlock")

	// ErrAlreadyRegistered means a UID cannot be assigned a second role.
	ErrAlreadyRegistered = errors.New("uid is already registered")
)

// HasMaster reports whether anything at all is on file, sentinel included.
// For real master cards, use GetMasterCount.
func (am *AuthManager) HasMaster() bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return len(am.masterUIDs) > 0
}

// IsMaster reports whether uid is a master card, which starts learn mode
// rather than unlocking.
func (am *AuthManager) IsMaster(uid string) bool {
	uid, err := NormalizeUID(uid)
	if err != nil || uid == MasterDisabled {
		return false
	}

	am.mu.RLock()
	defer am.mu.RUnlock()
	return contains(am.masterUIDs, uid)
}

// CanUnlock reports whether uid may grant access. Only authorized cards may.
func (am *AuthManager) CanUnlock(uid string) bool {
	uid, err := NormalizeUID(uid)
	if err != nil || uid == MasterDisabled {
		return false
	}

	am.mu.RLock()
	defer am.mu.RUnlock()
	return contains(am.authorizedUIDs, uid)
}

// IsKnown reports registration in either role. Kept separate from CanUnlock,
// which is the access decision.
func (am *AuthManager) IsKnown(uid string) bool {
	uid, err := NormalizeUID(uid)
	if err != nil || uid == MasterDisabled {
		return false
	}

	am.mu.RLock()
	defer am.mu.RUnlock()
	return contains(am.masterUIDs, uid) || contains(am.authorizedUIDs, uid)
}

func contains(list []string, uid string) bool {
	for _, e := range list {
		if e == uid {
			return true
		}
	}
	return false
}

// SetMaster replaces the master list. It does not touch authorized cards;
// wiping is what Reset is for. An authorized UID cannot become a master until
// it has been removed from the authorized list.
func (am *AuthManager) SetMaster(uid string) error {
	uid, err := NormalizeUID(uid)
	if err != nil {
		return err
	}

	am.mu.Lock()
	defer am.mu.Unlock()

	if uid != MasterDisabled && contains(am.authorizedUIDs, uid) {
		return ErrAlreadyRegistered
	}

	am.masterUIDs = []string{uid}
	return am.saveMasterUIDs()
}

// Reset clears and persists both lists together for installer start-over flows.
func (am *AuthManager) Reset() error {
	am.mu.Lock()
	defer am.mu.Unlock()

	am.masterUIDs = nil
	am.authorizedUIDs = nil

	if err := am.saveMasterUIDs(); err != nil {
		return err
	}
	return am.saveAuthorizedUIDs()
}

// AddMaster appends a master. False if uid is already registered in either role.
func (am *AuthManager) AddMaster(uid string) (bool, error) {
	uid, err := NormalizeUID(uid)
	if err != nil {
		return false, err
	}
	if uid == MasterDisabled {
		return false, ErrInvalidUID
	}

	am.mu.Lock()
	defer am.mu.Unlock()

	if contains(am.masterUIDs, uid) || contains(am.authorizedUIDs, uid) {
		return false, nil
	}

	am.masterUIDs = append(am.masterUIDs, uid)
	return true, am.saveMasterUIDs()
}

// RemoveMaster drops a master. Removing the last one is allowed: a vehicle
// with no master is recoverable, unlike one with no card that can unlock.
func (am *AuthManager) RemoveMaster(uid string) (bool, error) {
	uid, err := NormalizeUID(uid)
	if err != nil {
		return false, err
	}

	am.mu.Lock()
	defer am.mu.Unlock()

	for i, m := range am.masterUIDs {
		if m == uid {
			am.masterUIDs = append(am.masterUIDs[:i], am.masterUIDs[i+1:]...)
			return true, am.saveMasterUIDs()
		}
	}
	return false, nil
}

// ClearMasters empties the master list, sentinel included. The next start
// re-arms bootstrap.
func (am *AuthManager) ClearMasters() error {
	am.mu.Lock()
	defer am.mu.Unlock()

	am.masterUIDs = nil
	return am.saveMasterUIDs()
}

// ListMasters excludes the MasterDisabled sentinel.
func (am *AuthManager) ListMasters() []string {
	am.mu.RLock()
	defer am.mu.RUnlock()

	result := make([]string, 0, len(am.masterUIDs))
	for _, m := range am.masterUIDs {
		if m != MasterDisabled {
			result = append(result, m)
		}
	}
	return result
}

// AddAuthorized appends a card. False if uid is already registered in either role.
func (am *AuthManager) AddAuthorized(uid string) (bool, error) {
	uid, err := NormalizeUID(uid)
	if err != nil {
		return false, err
	}
	if uid == MasterDisabled {
		return false, ErrInvalidUID
	}

	am.mu.Lock()
	defer am.mu.Unlock()

	if contains(am.masterUIDs, uid) || contains(am.authorizedUIDs, uid) {
		return false, nil
	}

	am.authorizedUIDs = append(am.authorizedUIDs, uid)
	return true, am.saveAuthorizedUIDs()
}

// RemoveAuthorized drops a card, keeping at least one able to unlock.
// Membership is checked first so an absent card reads as not found.
func (am *AuthManager) RemoveAuthorized(uid string) (bool, error) {
	uid, err := NormalizeUID(uid)
	if err != nil {
		return false, err
	}

	am.mu.Lock()
	defer am.mu.Unlock()

	idx := -1
	for i, a := range am.authorizedUIDs {
		if a == uid {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}

	if len(am.authorizedUIDs) == 1 {
		return false, ErrLastCredential
	}

	am.authorizedUIDs = append(am.authorizedUIDs[:idx], am.authorizedUIDs[idx+1:]...)
	return true, am.saveAuthorizedUIDs()
}

func (am *AuthManager) ListAuthorized() []string {
	am.mu.RLock()
	defer am.mu.RUnlock()

	result := make([]string, len(am.authorizedUIDs))
	copy(result, am.authorizedUIDs)
	return result
}

func (am *AuthManager) GetAuthorizedCount() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return len(am.authorizedUIDs)
}

// GetMasterCount excludes the MasterDisabled sentinel.
func (am *AuthManager) GetMasterCount() int {
	am.mu.RLock()
	defer am.mu.RUnlock()
	n := 0
	for _, m := range am.masterUIDs {
		if m != MasterDisabled {
			n++
		}
	}
	return n
}

func (am *AuthManager) saveMasterUIDs() error {
	return atomicWriteLines(am.masterFilePath(), am.masterUIDs)
}

func (am *AuthManager) saveAuthorizedUIDs() error {
	return atomicWriteLines(am.authorizedFilePath(), am.authorizedUIDs)
}
