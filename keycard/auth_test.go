package keycard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const (
	master1 = "AABBCCDD"
	master2 = "AABBCCEE"
	user1   = "11223344"
	user2   = "11223355"
)

func newAM(t *testing.T) *AuthManager {
	t.Helper()
	am, err := NewAuthManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuthManager failed: %v", err)
	}
	return am
}

func mustAddAuthorized(t *testing.T, am *AuthManager, uid string) {
	t.Helper()
	added, err := am.AddAuthorized(uid)
	if err != nil {
		t.Fatalf("AddAuthorized(%s) failed: %v", uid, err)
	}
	if !added {
		t.Fatalf("AddAuthorized(%s) returned false", uid)
	}
}

func TestNormalizeUID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"aabbccdd", "AABBCCDD"},
		{"AA BB CC DD", "AABBCCDD"},
		{"aa:bb:cc:dd", "AABBCCDD"},
		{"AA-BB-CC-DD", "AABBCCDD"},
		{"  aabbccdd\t", "AABBCCDD"},
		{"04A1B2C3D4E5F6", "04A1B2C3D4E5F6"},
		{"NONE", MasterDisabled},
		{"none", MasterDisabled},
	}
	for _, c := range cases {
		got, err := NormalizeUID(c.in)
		if err != nil {
			t.Errorf("NormalizeUID(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeUID(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	bad := []string{"", "  ", "A", "ABC", "GG", "MASTER01", "aabbccddeeff00112233445566", "12 34 5"}
	for _, in := range bad {
		if got, err := NormalizeUID(in); !errors.Is(err, ErrInvalidUID) {
			t.Errorf("NormalizeUID(%q) = %q, %v; want ErrInvalidUID", in, got, err)
		}
	}
}

// A UID written with separators has to match a tap, which arrives as bare
// uppercase hex. Storing it verbatim is what made `add:04:A1:B2` over BLE
// reply ok and then never open the vehicle.
func TestAddAuthorized_NormalizesSeparators(t *testing.T) {
	am := newAM(t)
	mustAddAuthorized(t, am, "11:22:33:44")

	if !am.CanUnlock(user1) {
		t.Error("a UID added with separators should match the bare hex tap")
	}
	if got := am.ListAuthorized(); len(got) != 1 || got[0] != user1 {
		t.Errorf("stored %v, want [%s]", got, user1)
	}
}

func TestAddAuthorized_RejectsMalformed(t *testing.T) {
	am := newAM(t)
	for _, uid := range []string{"", "NOTHEX", MasterDisabled} {
		added, err := am.AddAuthorized(uid)
		if added || err == nil {
			t.Errorf("AddAuthorized(%q) = %v, %v; want refusal", uid, added, err)
		}
	}
	if am.GetAuthorizedCount() != 0 {
		t.Error("no malformed UID should have been stored")
	}
}

// Masters start learn mode. They never grant access, so CanUnlock must say
// no for them even though IsKnown says yes.
func TestMasterCannotUnlock(t *testing.T) {
	am := newAM(t)
	if err := am.SetMaster(master1); err != nil {
		t.Fatalf("SetMaster failed: %v", err)
	}
	mustAddAuthorized(t, am, user1)

	if !am.IsMaster(master1) {
		t.Error("IsMaster should be true for the master")
	}
	if am.CanUnlock(master1) {
		t.Error("a master card must not be able to unlock")
	}
	if !am.IsKnown(master1) {
		t.Error("a master card is known")
	}

	if !am.CanUnlock(user1) {
		t.Error("an authorized card unlocks")
	}
	if am.CanUnlock(user2) {
		t.Error("an unknown card does not unlock")
	}
	if am.IsKnown(user2) {
		t.Error("an unknown card is not known")
	}
	if !am.CanUnlock("11 22 33 44") {
		t.Error("CanUnlock should normalize its argument")
	}
}

// SetMaster used to wipe the authorized list, which turned every master
// change into a silent revocation of every rider's card.
func TestSetMaster_KeepsAuthorized(t *testing.T) {
	am := newAM(t)
	if err := am.SetMaster(master1); err != nil {
		t.Fatalf("SetMaster failed: %v", err)
	}
	mustAddAuthorized(t, am, user1)
	mustAddAuthorized(t, am, user2)

	if err := am.SetMaster(master2); err != nil {
		t.Fatalf("SetMaster failed: %v", err)
	}

	if am.GetAuthorizedCount() != 2 {
		t.Errorf("authorized cards should survive a master change, got %d", am.GetAuthorizedCount())
	}
	if am.IsMaster(master1) {
		t.Error("the replaced master should no longer be master")
	}
	if !am.IsMaster(master2) {
		t.Error("the new master should be master")
	}
}

func TestSetMaster_Disabled(t *testing.T) {
	am := newAM(t)
	mustAddAuthorized(t, am, user1)

	if err := am.SetMaster(MasterDisabled); err != nil {
		t.Fatalf("SetMaster(NONE) failed: %v", err)
	}

	// The sentinel suppresses boot-time bootstrap without being a card.
	if !am.HasMaster() {
		t.Error("the sentinel should count as a decision about masters")
	}
	if am.GetMasterCount() != 0 {
		t.Errorf("the sentinel is not a master card, got count %d", am.GetMasterCount())
	}
	if len(am.ListMasters()) != 0 {
		t.Error("ListMasters should not report the sentinel")
	}
	if am.IsMaster(MasterDisabled) {
		t.Error("the sentinel must never match a card")
	}
	if am.GetAuthorizedCount() != 1 {
		t.Error("disabling the master must not revoke cards")
	}
}

func TestMasterPrimitives(t *testing.T) {
	am := newAM(t)
	mustAddAuthorized(t, am, user1)

	added, err := am.AddMaster(master1)
	if err != nil || !added {
		t.Fatalf("AddMaster = %v, %v", added, err)
	}
	added, err = am.AddMaster("aa:bb:cc:ee")
	if err != nil || !added {
		t.Fatalf("AddMaster(second) = %v, %v", added, err)
	}
	if got := am.ListMasters(); len(got) != 2 || got[1] != master2 {
		t.Errorf("ListMasters = %v", got)
	}

	// A UID already registered in either role cannot be added again.
	if added, _ := am.AddMaster(master1); added {
		t.Error("AddMaster should refuse an existing master")
	}
	if added, _ := am.AddMaster(user1); added {
		t.Error("AddMaster should refuse an authorized card")
	}
	if added, _ := am.AddAuthorized(master1); added {
		t.Error("AddAuthorized should refuse a master")
	}

	removed, err := am.RemoveMaster(master1)
	if err != nil || !removed {
		t.Fatalf("RemoveMaster = %v, %v", removed, err)
	}
	if removed, _ := am.RemoveMaster(master1); removed {
		t.Error("RemoveMaster should report false for an absent master")
	}

	// Emptying the master list is allowed: it is recoverable, unlike having
	// no card that can unlock.
	if removed, err := am.RemoveMaster(master2); err != nil || !removed {
		t.Fatalf("RemoveMaster(last) = %v, %v", removed, err)
	}
	if am.HasMaster() {
		t.Error("master list should be empty")
	}
	if am.GetAuthorizedCount() != 1 {
		t.Error("removing masters must not touch authorized cards")
	}
}

func TestClearMasters(t *testing.T) {
	am := newAM(t)
	if err := am.SetMaster(master1); err != nil {
		t.Fatalf("SetMaster failed: %v", err)
	}
	mustAddAuthorized(t, am, user1)

	if err := am.ClearMasters(); err != nil {
		t.Fatalf("ClearMasters failed: %v", err)
	}
	if am.HasMaster() {
		t.Error("expected no master after ClearMasters")
	}
	if am.GetAuthorizedCount() != 1 {
		t.Error("ClearMasters must not touch authorized cards")
	}
}

// Membership is checked before the anti-lockout invariant, so removing a card
// the vehicle does not have reads as absent rather than as a lockout refusal.
func TestRemoveAuthorized_Invariant(t *testing.T) {
	am := newAM(t)
	if err := am.SetMaster(master1); err != nil {
		t.Fatalf("SetMaster failed: %v", err)
	}
	mustAddAuthorized(t, am, user1)

	removed, err := am.RemoveAuthorized(user2)
	if removed || err != nil {
		t.Errorf("removing an absent card = %v, %v; want false, nil", removed, err)
	}

	removed, err = am.RemoveAuthorized(user1)
	if removed || !errors.Is(err, ErrLastCredential) {
		t.Errorf("removing the only card = %v, %v; want ErrLastCredential", removed, err)
	}

	// Masters do not count towards the invariant: they cannot unlock.
	if added, _ := am.AddMaster(master2); !added {
		t.Fatal("AddMaster failed")
	}
	if removed, err := am.RemoveAuthorized(user1); removed || !errors.Is(err, ErrLastCredential) {
		t.Errorf("a second master must not satisfy the invariant: %v, %v", removed, err)
	}

	// Adding a second card does.
	mustAddAuthorized(t, am, user2)
	if removed, err := am.RemoveAuthorized("11 22 33 44"); !removed || err != nil {
		t.Errorf("RemoveAuthorized = %v, %v; want true, nil", removed, err)
	}
	if am.CanUnlock(user1) {
		t.Error("the removed card should no longer unlock")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()

	am1, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("NewAuthManager failed: %v", err)
	}
	if err := am1.SetMaster(master1); err != nil {
		t.Fatalf("SetMaster failed: %v", err)
	}
	mustAddAuthorized(t, am1, user1)
	mustAddAuthorized(t, am1, user2)

	am2, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("NewAuthManager (reload) failed: %v", err)
	}
	if !am2.IsMaster(master1) {
		t.Error("expected the master to persist")
	}
	if !am2.CanUnlock(user1) || !am2.CanUnlock(user2) {
		t.Error("expected authorized cards to persist")
	}
	if am2.GetAuthorizedCount() != 2 {
		t.Errorf("expected 2 authorized UIDs after reload, got %d", am2.GetAuthorizedCount())
	}
}

func TestLoad_NormalizesAndRejects(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "master_uids.txt"),
		[]byte("AA BB CC DD\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "authorized_uids.txt"),
		[]byte("11:22:33:44\n\n# a comment\nNOTHEX\n11223355\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	am, err := NewAuthManager(dir)
	if err != nil {
		t.Fatalf("NewAuthManager failed: %v", err)
	}

	if !am.IsMaster(master1) {
		t.Error("expected the master to match after normalizing separators")
	}
	if !am.CanUnlock(user1) || !am.CanUnlock(user2) {
		t.Error("expected both well-formed cards to load")
	}
	if am.GetAuthorizedCount() != 2 {
		t.Errorf("expected 2 authorized UIDs, got %d", am.GetAuthorizedCount())
	}

	// A dropped line is reported rather than lost, since the next write
	// removes it from the file.
	rejected := am.Rejected()
	if len(rejected) != 1 || rejected[0] != "NOTHEX" {
		t.Errorf("Rejected() = %v, want [NOTHEX]", rejected)
	}
}

func TestReset(t *testing.T) {
	am := newAM(t)
	if err := am.SetMaster(master1); err != nil {
		t.Fatalf("SetMaster failed: %v", err)
	}
	mustAddAuthorized(t, am, user1)

	if err := am.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}
	if am.HasMaster() || am.GetAuthorizedCount() != 0 {
		t.Error("Reset should wipe both lists")
	}
}
