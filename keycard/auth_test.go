package keycard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuthManager_MasterUID(t *testing.T) {
	dir := t.TempDir()

	am, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("NewAuthManager failed: %v", err)
	}

	// Initially no master
	if am.HasMaster() {
		t.Error("expected no master initially")
	}

	// Set master
	if err := am.SetMaster("AABBCCDD"); err != nil {
		t.Fatalf("SetMaster failed: %v", err)
	}

	if !am.HasMaster() {
		t.Error("expected HasMaster to be true after SetMaster")
	}

	if !am.IsMaster("AABBCCDD") {
		t.Error("expected IsMaster to return true for master UID")
	}

	if !am.IsMaster("aabbccdd") {
		t.Error("expected IsMaster to be case-insensitive")
	}

	if am.IsMaster("11223344") {
		t.Error("expected IsMaster to return false for non-master UID")
	}
}

func TestAuthManager_AuthorizedUIDs(t *testing.T) {
	dir := t.TempDir()

	am, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("NewAuthManager failed: %v", err)
	}

	// Set master first
	if err := am.SetMaster("MASTER01"); err != nil {
		t.Fatalf("SetMaster failed: %v", err)
	}

	// Add authorized UID
	added, err := am.AddAuthorized("USER0001")
	if err != nil {
		t.Fatalf("AddAuthorized failed: %v", err)
	}
	if !added {
		t.Error("expected AddAuthorized to return true for new UID")
	}

	// Check authorization
	if !am.IsAuthorized("USER0001") {
		t.Error("expected IsAuthorized to return true for authorized UID")
	}

	if !am.IsAuthorized("user0001") {
		t.Error("expected IsAuthorized to be case-insensitive")
	}

	// Master should also be authorized
	if !am.IsAuthorized("MASTER01") {
		t.Error("expected master to be authorized")
	}

	// Unknown UID should not be authorized
	if am.IsAuthorized("UNKNOWN1") {
		t.Error("expected IsAuthorized to return false for unknown UID")
	}

	// Adding same UID again should return false
	added, err = am.AddAuthorized("USER0001")
	if err != nil {
		t.Fatalf("AddAuthorized failed: %v", err)
	}
	if added {
		t.Error("expected AddAuthorized to return false for existing UID")
	}

	// Adding master as authorized should return false
	added, err = am.AddAuthorized("MASTER01")
	if err != nil {
		t.Fatalf("AddAuthorized failed: %v", err)
	}
	if added {
		t.Error("expected AddAuthorized to return false for master UID")
	}

	if am.GetAuthorizedCount() != 1 {
		t.Errorf("expected 1 authorized UID, got %d", am.GetAuthorizedCount())
	}
}

func TestAuthManager_SetMasterClearsAuthorized(t *testing.T) {
	dir := t.TempDir()

	am, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("NewAuthManager failed: %v", err)
	}

	// Set master and add authorized
	am.SetMaster("MASTER01")
	am.AddAuthorized("USER0001")
	am.AddAuthorized("USER0002")

	if am.GetAuthorizedCount() != 2 {
		t.Errorf("expected 2 authorized UIDs, got %d", am.GetAuthorizedCount())
	}

	// Setting new master should clear authorized
	am.SetMaster("MASTER02")

	if am.GetAuthorizedCount() != 0 {
		t.Errorf("expected 0 authorized UIDs after new master, got %d", am.GetAuthorizedCount())
	}

	if am.IsMaster("MASTER01") {
		t.Error("old master should no longer be master")
	}

	if !am.IsMaster("MASTER02") {
		t.Error("new master should be master")
	}
}

func TestAuthManager_Persistence(t *testing.T) {
	dir := t.TempDir()

	// Create and populate
	am1, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("NewAuthManager failed: %v", err)
	}

	am1.SetMaster("MASTER01")
	am1.AddAuthorized("USER0001")
	am1.AddAuthorized("USER0002")

	// Create new instance from same directory
	am2, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("NewAuthManager (reload) failed: %v", err)
	}

	if !am2.HasMaster() {
		t.Error("expected master to persist")
	}

	if !am2.IsMaster("MASTER01") {
		t.Error("expected master UID to persist")
	}

	if !am2.IsAuthorized("USER0001") {
		t.Error("expected authorized UID to persist")
	}

	if !am2.IsAuthorized("USER0002") {
		t.Error("expected authorized UID to persist")
	}

	if am2.GetAuthorizedCount() != 2 {
		t.Errorf("expected 2 authorized UIDs after reload, got %d", am2.GetAuthorizedCount())
	}
}

func TestAuthManager_NormalizesUIDs(t *testing.T) {
	dir := t.TempDir()

	// Write file with spaces (as might come from manual editing)
	masterFile := filepath.Join(dir, "master_uids.txt")
	os.WriteFile(masterFile, []byte("AA BB CC DD\n"), 0644)

	authFile := filepath.Join(dir, "authorized_uids.txt")
	os.WriteFile(authFile, []byte("11 22 33 44\n"), 0644)

	am, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("NewAuthManager failed: %v", err)
	}

	// Should match without spaces
	if !am.IsMaster("AABBCCDD") {
		t.Error("expected master to match after normalizing spaces")
	}

	if !am.IsAuthorized("11223344") {
		t.Error("expected authorized to match after normalizing spaces")
	}
}
