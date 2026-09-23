package keycard

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyAliasesPersistAndValidate(t *testing.T) {
	longestCommand := "keycard:alias:set:phone:" + strings.Repeat("A", 32) + ":" +
		base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", maxAliasBytes)))
	if len(longestCommand) != 100 {
		t.Fatalf("longest BLE alias command = %d bytes", len(longestCommand))
	}
	dir := t.TempDir()
	aliases, err := newKeyAliases(dir)
	if err != nil {
		t.Fatal(err)
	}
	card, err := aliasKey("card", "04:01:02:03")
	if err != nil || card != "card:04010203" {
		t.Fatalf("card key = %q, %v", card, err)
	}
	phone, err := aliasKey("phone", strings.Repeat("ab", 16))
	if err != nil || phone != "phone:"+strings.Repeat("AB", 16) {
		t.Fatalf("phone key = %q, %v", phone, err)
	}
	if _, err := aliasKey("card", MasterDisabled); !errors.Is(err, errInvalidAlias) {
		t.Fatalf("disabled master accepted: %v", err)
	}
	for _, name := range []string{"", "  ", "two\nlines", strings.Repeat("x", maxAliasBytes+1)} {
		if err := aliases.set(card, name); !errors.Is(err, errInvalidAlias) {
			t.Fatalf("accepted name %q: %v", name, err)
		}
	}
	if err := aliases.set(card, " Spare: 🛵 "); err != nil {
		t.Fatal(err)
	}
	if err := aliases.set(phone, "My phone"); err != nil {
		t.Fatal(err)
	}
	loaded, err := newKeyAliases(dir)
	if err != nil || loaded.list()[card] != "Spare: 🛵" || loaded.list()[phone] != "My phone" {
		t.Fatalf("reloaded aliases = %v, %v", loaded.list(), err)
	}
	if err := loaded.prune(map[string]bool{phone: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.list()[card]; ok {
		t.Fatal("removed card name survived pruning")
	}
	if err := loaded.clear(phone); err != nil {
		t.Fatal(err)
	}
	if got, err := newKeyAliases(dir); err != nil || len(got.list()) != 0 {
		t.Fatalf("clear not persisted: %v, %v", got.list(), err)
	}
}

func TestKeyAliasWriteFailureAndCorruptFileNeverTrustPartialData(t *testing.T) {
	dir := t.TempDir()
	aliases, err := newKeyAliases(dir)
	if err != nil {
		t.Fatal(err)
	}
	aliases.path = filepath.Join(dir, "missing", "aliases.json")
	if err := aliases.set("card:04010203", "Spare"); err == nil || len(aliases.list()) != 0 {
		t.Fatalf("failed write kept alias: %v, %v", err, aliases.list())
	}
	if err := os.WriteFile(filepath.Join(dir, "key_aliases.json"), []byte(`{"card:04010203":"Valid","phone:bad":"Bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := newKeyAliases(dir)
	if err == nil || len(loaded.list()) != 0 {
		t.Fatalf("partial file trusted: %v, %v", err, loaded.list())
	}
}

func TestExistingCredentialFilesRemainReadableWithOptionalAliases(t *testing.T) {
	dir := t.TempDir()
	der, _, _ := makePhoneProof(t)
	files := map[string][]byte{
		"master_uids.txt":     []byte("AABBCCDD\n"),
		"authorized_uids.txt": []byte("04010203\n"),
		"phone_keys.txt":      []byte(hex.EncodeToString(der) + "\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	aliases, err := newKeyAliases(dir)
	if err != nil || len(aliases.list()) != 0 {
		t.Fatalf("missing optional names file: %v, %v", err, aliases.list())
	}
	if _, err := os.Stat(filepath.Join(dir, "key_aliases.json")); !os.IsNotExist(err) {
		t.Fatalf("missing names file was created: %v", err)
	}
	if err := aliases.set("card:04010203", "Spare"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "key_aliases.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("names file permissions: %v, %v", info, err)
	}
	for name, original := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != string(original) {
			t.Fatalf("credential file %s changed: %q, %v", name, data, err)
		}
	}
	auth, err := NewAuthManager(dir)
	if err != nil || !auth.IsMaster("AABBCCDD") || !auth.CanUnlock("04010203") {
		t.Fatalf("physical credentials failed to load: %v", err)
	}
	phones, err := newPhoneKeys(dir)
	if err != nil || !phones.has(der) {
		t.Fatalf("phone credential failed to load: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key_aliases.json"), []byte("not JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newKeyAliases(dir); err == nil {
		t.Fatal("invalid names file accepted")
	}
	auth, err = NewAuthManager(dir)
	if err != nil || !auth.IsMaster("AABBCCDD") || !auth.CanUnlock("04010203") {
		t.Fatalf("invalid names disabled physical credentials: %v", err)
	}
	phones, err = newPhoneKeys(dir)
	if err != nil || !phones.has(der) {
		t.Fatalf("invalid names disabled phone credentials: %v", err)
	}
}

func TestKeyAliasCommandAndDashboardSnapshot(t *testing.T) {
	service, server := newExclusivityTestService(t)
	aliases, err := newKeyAliases(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service.aliases = aliases
	phones, err := newPhoneKeys(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service.phones = phones
	const uid = "04010203"
	if _, err := service.auth.AddAuthorized(uid); err != nil {
		t.Fatal(err)
	}
	der, _, _ := makePhoneProof(t)
	if err := phones.add(der); err != nil {
		t.Fatal(err)
	}
	fingerprint := phoneFingerprint(der)
	service.handleAliasMutation("card:"+uid+":"+base64.RawURLEncoding.EncodeToString([]byte("Spare: card")), false)
	service.handleAliasMutation("phone:"+fingerprint+":"+base64.RawURLEncoding.EncodeToString([]byte("My phone")), false)
	if got := server.HGet("keycard", "command-result"); got != resultOK {
		t.Fatalf("set result = %q", got)
	}
	members, err := server.Members("keycard:aliases")
	if err != nil || len(members) != 2 {
		t.Fatalf("dashboard names = %v, %v", members, err)
	}
	service.handleAliasMutation("card:FFFF:"+base64.RawURLEncoding.EncodeToString([]byte("Unknown")), false)
	if got := server.HGet("keycard", "command-error"); got != codeNotFound {
		t.Fatalf("unknown credential result = %q", got)
	}
	service.handleAliasMutation("card:"+uid+":not*base64", false)
	if got := server.HGet("keycard", "command-error"); got != codeInvalidAlias {
		t.Fatalf("invalid name result = %q", got)
	}
	service.handleAliasList()
	if got := server.HGet("keycard", "command-result"); !strings.HasPrefix(got, "alias:phone:"+fingerprint+":") {
		t.Fatalf("list terminal entry = %q", got)
	}
	if _, err := phones.remove(fingerprint); err != nil {
		t.Fatal(err)
	}
	service.publishKeycardSnapshot()
	members, err = server.Members("keycard:aliases")
	if err != nil || len(members) != 1 || members[0] != "card:"+uid+":Spare: card" {
		t.Fatalf("names after phone removal = %v, %v", members, err)
	}
	service.handleAliasMutation("card:"+uid, true)
	if server.Exists("keycard:aliases") {
		t.Fatal("cleared name remained on dashboard")
	}
}
