package keycard

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"testing"

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
	if !phone || err == nil {
		t.Fatal("reader failure must not fall back to UID")
	}
}
