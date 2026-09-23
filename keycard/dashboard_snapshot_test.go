package keycard

import (
	"slices"
	"testing"
)

func TestDashboardPhoneSnapshotAndLastUsedCard(t *testing.T) {
	service, server := newExclusivityTestService(t)
	phones, err := newPhoneKeys(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service.phones = phones
	der, _, _ := makePhoneProof(t)
	if err := phones.add(der); err != nil {
		t.Fatal(err)
	}
	const uid = "04010203"
	if _, err := service.auth.AddAuthorized(uid); err != nil {
		t.Fatal(err)
	}

	service.publishKeycardSnapshot()
	members, err := server.Members("keycard:phones")
	if err != nil || !slices.Equal(members, []string{phoneFingerprint(der)}) {
		t.Fatalf("phone snapshot = %v, %v", members, err)
	}

	service.grantAccess(uid)
	if got := server.HGet("system", "keycard-last-used-uid"); got != uid {
		t.Fatalf("last used card = %q", got)
	}
	service.grantAccess("PHONE-" + phoneFingerprint(der))
	if got := server.HGet("system", "keycard-last-used-uid"); got != uid {
		t.Fatalf("phone unlock replaced physical last used card: %q", got)
	}

	if _, err := phones.remove(phoneFingerprint(der)); err != nil {
		t.Fatal(err)
	}
	service.publishKeycardSnapshot()
	if server.Exists("keycard:phones") {
		t.Fatal("removed phone remains in dashboard snapshot")
	}
	if _, err := service.auth.RemoveAuthorized(uid, true); err != nil {
		t.Fatal(err)
	}
	service.publishKeycardSnapshot()
	if got := server.HGet("system", "keycard-last-used-uid"); got != "" {
		t.Fatalf("removed card remains last used: %q", got)
	}
}
