package keycard

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hal "github.com/librescoot/pn7150"
)

func makePhoneProof(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	challenge := bytes.Repeat([]byte{0x55}, 32)
	digest := phoneDigest(challenge)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	resp := append(append([]byte{1}, der...), sig...)
	resp = append(resp, 0x90, 0x00)
	return der, challenge, resp
}

func TestPhoneChallengeResponse(t *testing.T) {
	der, challenge, resp := makePhoneProof(t)
	got, err := verifyPhoneResponse(resp, challenge)
	if err != nil || !bytes.Equal(der, got) {
		t.Fatalf("valid proof: %v", err)
	}
	other := bytes.Repeat([]byte{0x56}, 32)
	if _, err := verifyPhoneResponse(resp, other); err == nil {
		t.Fatal("replayed proof accepted")
	}
	resp[len(resp)-3] ^= 1
	if _, err := verifyPhoneResponse(resp, challenge); err == nil {
		t.Fatal("tampered signature accepted")
	}
	wrongDomain := sha256.Sum256(challenge)
	correct := phoneDigest(challenge)
	if bytes.Equal(wrongDomain[:], correct[:]) {
		t.Fatal("missing domain separation")
	}
}

func TestPhoneEnrollmentAndRevocation(t *testing.T) {
	der, _, _ := makePhoneProof(t)
	dir := t.TempDir()
	store, err := newPhoneKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store.has(der) {
		t.Fatal("unregistered key accepted")
	}
	if err := store.add(der); err != nil {
		t.Fatal(err)
	}
	store, err = newPhoneKeys(dir)
	if err != nil || !store.has(der) {
		t.Fatalf("key did not persist: %v", err)
	}
	id := phoneFingerprint(der)
	if len(id) != 32 {
		t.Fatal("unexpected fingerprint length")
	}
	removed, err := store.remove(id)
	if err != nil || !removed {
		t.Fatalf("revoke: %v", err)
	}
	store, err = newPhoneKeys(dir)
	if err != nil || store.has(der) {
		t.Fatalf("revoked key survived reload: %v", err)
	}
}

type apduTestReader struct {
	nfcReader
	respond func([]byte) ([]byte, error)
}

func (r apduTestReader) ExchangeAPDU(apdu []byte) ([]byte, error) { return r.respond(apdu) }

func TestPendingPhoneDuplicatePublishesFeedback(t *testing.T) {
	service, _ := newExclusivityTestService(t)
	phones, err := newPhoneKeys(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service.phones = phones
	service.learnMode = true
	der, _, _ := makePhoneProof(t)
	ctx := context.Background()
	listener := service.redis.client.Raw().Subscribe(ctx, "keycard:events")
	defer listener.Close()
	if _, err := listener.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"phone-learned:", "phone-duplicate:"} {
		service.handlePhone(der)
		waitCtx, cancel := context.WithTimeout(ctx, time.Second)
		msg, err := listener.ReceiveMessage(waitCtx)
		cancel()
		if err != nil || !strings.HasPrefix(msg.Payload, expected) {
			t.Fatalf("event = %v, want %q: %v", msg, expected, err)
		}
	}
	if len(service.newPhones) != 1 {
		t.Fatalf("pending phone count = %d", len(service.newPhones))
	}
}

func TestPhoneSelectRequiresISO_DEP(t *testing.T) {
	service := &Service{}
	phone, _, err := service.readPhoneProof(hal.Tag{RFProtocol: hal.RFProtocolT2T})
	if phone || err != nil {
		t.Fatalf("unexpected T2T phone: %v", err)
	}
}

func TestPhoneProofReaderFlow(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	service := &Service{nfc: apduTestReader{respond: func(apdu []byte) ([]byte, error) {
		calls++
		if calls == 1 {
			if !bytes.Equal(apdu[5:13], phoneAID) {
				t.Fatalf("wrong AID: %X", apdu)
			}
			return []byte{0x90, 0x00}, nil
		}
		if len(apdu) != 37 || !bytes.Equal(apdu[:5], []byte{0x80, 0x10, 0, 0, 32}) {
			t.Fatalf("wrong challenge APDU: %X", apdu)
		}
		digest := phoneDigest(apdu[5:])
		sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return append(append(append([]byte{1}, der...), sig...), 0x90, 0), nil
	}}}
	phone, got, err := service.readPhoneProof(hal.Tag{RFProtocol: hal.RFProtocolISODEP})
	if err != nil || !phone || !bytes.Equal(got, der) || calls != 2 {
		t.Fatalf("proof failed: phone=%t calls=%d err=%v", phone, calls, err)
	}
	service.nfc = apduTestReader{respond: func([]byte) ([]byte, error) { return nil, errors.New("reader failed") }}
	phone, _, err = service.readPhoneProof(hal.Tag{RFProtocol: hal.RFProtocolISODEP})
	if phone || err == nil {
		t.Fatal("failed SELECT should not claim to be a selected phone")
	}
}

func TestLegacyPhysicalCardEnrollmentSurvivesPhoneSelectFailure(t *testing.T) {
	for _, mode := range []string{"learn", "bootstrap", "teach-in"} {
		t.Run(mode, func(t *testing.T) {
			service, _ := newExclusivityTestService(t)
			service.nfc = apduTestReader{respond: func([]byte) ([]byte, error) {
				return nil, errors.New("legacy ISO-DEP SELECT not supported")
			}}
			switch mode {
			case "learn":
				service.learnMode = true
			case "bootstrap":
				service.masterBootstrapMode = true
			case "teach-in":
				service.masterTeachInMode = true
			}
			service.handleDetectedTag(hal.Tag{RFProtocol: hal.RFProtocolISODEP, ID: []byte{0x04, 0x01, 0x02, 0x03}})
			switch mode {
			case "learn":
				if len(service.newUIDs) != 1 || service.newUIDs[0] != "04010203" {
					t.Fatalf("legacy card not queued in learn mode: %v", service.newUIDs)
				}
			default:
				if !service.auth.IsMaster("04010203") {
					t.Fatal("legacy ISO-DEP card not enrolled as master")
				}
			}
		})
	}
}

func TestSelectedPhoneFailureNeverFallsBackToUID(t *testing.T) {
	service, _ := newExclusivityTestService(t)
	service.learnMode = true
	calls := 0
	service.nfc = apduTestReader{respond: func([]byte) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte{0x90, 0x00}, nil
		}
		return nil, errors.New("invalid phone proof")
	}}
	service.handleDetectedTag(hal.Tag{RFProtocol: hal.RFProtocolISODEP, ID: []byte{0x04, 0x01, 0x02, 0x03}})
	if len(service.newUIDs) != 0 || calls != 2 {
		t.Fatal("failed phone was enrolled as a legacy UID")
	}
}

func TestDamagedPhoneStoreDisablesPhonesButNotPhysicalCards(t *testing.T) {
	der, _, _ := makePhoneProof(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "phone_keys.txt")
	contents := []byte(hex.EncodeToString(der) + "\nNOT-A-PUBLIC-KEY\n")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := newPhoneKeys(dir)
	if err == nil || store == nil || store.health() == nil {
		t.Fatalf("expected disabled store, got %v", err)
	}
	if store.has(der) || len(store.list()) != 0 {
		t.Fatal("partially loaded phone key was trusted")
	}
	if err := store.add(der); err == nil {
		t.Fatal("damaged file overwritten during enrollment")
	}
	if _, err := store.remove(phoneFingerprint(der)); err == nil {
		t.Fatal("damaged file overwritten during revocation")
	}
	if err := store.clear(); err == nil {
		t.Fatal("damaged file overwritten during reset")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("damaged file changed: %v", err)
	}

	server := miniredis.RunT(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := NewService(&Config{DataDir: dir, RedisAddr: server.Addr()}, logger)
	if err != nil {
		t.Fatalf("physical service failed to start: %v", err)
	}
	t.Cleanup(func() { _ = service.redis.Close(); _ = service.rgbLed.Close() })
	if _, err := service.auth.AddAuthorized("04010203"); err != nil {
		t.Fatal(err)
	}
	service.handleDetectedTag(hal.Tag{RFProtocol: hal.RFProtocolISODEP, ID: []byte{4, 1, 2, 3}})
	deadline := time.Now().Add(time.Second)
	for server.HGet("keycard", "authentication") != "passed" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := server.HGet("keycard", "authentication"); got != "passed" {
		t.Fatalf("physical access = %q", got)
	}
	if err := service.resetAll(); err == nil {
		t.Fatal("reset falsely claimed it had revoked unreadable phone credentials")
	}
	if !service.auth.CanUnlock("04010203") {
		t.Fatal("failed phone reset cleared physical credentials")
	}
}

func TestUnreadablePhoneStoreDoesNotPreventPhysicalServiceStartup(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "phone_keys.txt"), 0700); err != nil {
		t.Fatal(err)
	}
	server := miniredis.RunT(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := NewService(&Config{DataDir: dir, RedisAddr: server.Addr()}, logger)
	if err != nil {
		t.Fatalf("physical service failed to start: %v", err)
	}
	t.Cleanup(func() { _ = service.redis.Close(); _ = service.rgbLed.Close() })
	if service.phones.health() == nil {
		t.Fatal("unreadable phone store must be disabled")
	}
	reader := &recoveryTestNFC{started: make(chan struct{}), unblock: make(chan struct{}), awaitError: errors.New("reader stopped")}
	service.nfcFactory = func(*Config, *slog.Logger) (nfcReader, error) { return reader, nil }
	service.faults = &recoveryTestFaults{}
	service.watchCommands = func(context.Context) {}
	runDone := runUntilStarted(t, service, reader)
	stopRecoveryTestService(t, service, reader, runDone)
	if service.masterBootstrapMode {
		t.Fatal("corrupt phone storage was mistaken for factory-fresh state")
	}
}

func TestKnownPhysicalCardSkipsPhoneProbe(t *testing.T) {
	service, server := newExclusivityTestService(t)
	if _, err := service.auth.AddAuthorized("04010203"); err != nil {
		t.Fatal(err)
	}
	service.nfc = apduTestReader{respond: func([]byte) ([]byte, error) {
		t.Fatal("existing physical credential must not be probed")
		return nil, nil
	}}
	service.handleDetectedTag(hal.Tag{RFProtocol: hal.RFProtocolISODEP, ID: []byte{0x04, 0x01, 0x02, 0x03}})
	deadline := time.Now().Add(time.Second)
	for server.HGet("keycard", "authentication") != "passed" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := server.HGet("keycard", "authentication"); got != "passed" {
		t.Fatalf("physical access = %q", got)
	}
}
