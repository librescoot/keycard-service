package keycard

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	hal "github.com/librescoot/pn7150"
)

// Phone protocol v1: SELECT F04C53434F4F5401, then
// 80 10 00 00 20 <32 random bytes>. The phone returns
// 01 <91-byte P-256 SPKI> <DER ECDSA-SHA256 signature> 90 00.
// Both sides sign SHA256(domain || challenge); the scooter's per-tap random
// challenge makes recorded NFC traffic useless for a later tap.
var phoneAID = []byte{0xF0, 0x4C, 0x53, 0x43, 0x4F, 0x4F, 0x54, 0x01}
var phoneDomain = []byte("librescoot-phone-unlock-v1\x00")

const phoneSPKILength = 91

type phoneKeys struct {
	mu   sync.RWMutex
	path string
	keys map[string][]byte // fingerprint -> DER SubjectPublicKeyInfo
}

func phoneFingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return strings.ToUpper(hex.EncodeToString(h[:16]))
}

func validatePhoneKey(der []byte) error {
	if len(der) != phoneSPKILength {
		return errors.New("invalid phone key length")
	}
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return err
	}
	ec, ok := key.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return errors.New("phone key must be P-256")
	}
	return nil
}

func newPhoneKeys(dir string) (*phoneKeys, error) {
	p := &phoneKeys{path: filepath.Join(dir, "phone_keys.txt"), keys: make(map[string][]byte)}
	data, err := os.ReadFile(p.path)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		der, err := hex.DecodeString(line)
		if err != nil || validatePhoneKey(der) != nil {
			return nil, fmt.Errorf("invalid enrolled phone key file")
		}
		p.keys[phoneFingerprint(der)] = der
	}
	return p, nil
}

func (p *phoneKeys) has(der []byte) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	stored, ok := p.keys[phoneFingerprint(der)]
	return ok && string(stored) == string(der)
}

func (p *phoneKeys) list() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]string, 0, len(p.keys))
	for id := range p.keys {
		result = append(result, id)
	}
	return result
}

func (p *phoneKeys) save() error {
	lines := make([]string, 0, len(p.keys))
	for _, der := range p.keys {
		lines = append(lines, strings.ToUpper(hex.EncodeToString(der)))
	}
	return atomicWriteLines(p.path, lines)
}

func (p *phoneKeys) add(der []byte) error {
	if err := validatePhoneKey(der); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	id := phoneFingerprint(der)
	if _, ok := p.keys[id]; ok {
		return nil
	}
	p.keys[id] = append([]byte(nil), der...)
	if err := p.save(); err != nil {
		delete(p.keys, id)
		return err
	}
	return nil
}

func (p *phoneKeys) clear() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	previous := p.keys
	p.keys = make(map[string][]byte)
	if err := p.save(); err != nil {
		p.keys = previous
		return err
	}
	return nil
}

func (p *phoneKeys) remove(id string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id = strings.ToUpper(id)
	old, ok := p.keys[id]
	if !ok {
		return false, nil
	}
	delete(p.keys, id)
	if err := p.save(); err != nil {
		p.keys[id] = old
		return false, err
	}
	return true, nil
}

func phoneDigest(challenge []byte) [32]byte {
	msg := append(append([]byte(nil), phoneDomain...), challenge...)
	return sha256.Sum256(msg)
}

func verifyPhoneResponse(resp, challenge []byte) ([]byte, error) {
	if len(challenge) != 32 || len(resp) < 1+phoneSPKILength+8+2 || len(resp) > 1+phoneSPKILength+80+2 ||
		resp[0] != 1 || resp[len(resp)-2] != 0x90 || resp[len(resp)-1] != 0 {
		return nil, errors.New("invalid phone response")
	}
	der := resp[1 : 1+phoneSPKILength]
	if err := validatePhoneKey(der); err != nil {
		return nil, err
	}
	key, _ := x509.ParsePKIXPublicKey(der)
	digest := phoneDigest(challenge)
	if !ecdsa.VerifyASN1(key.(*ecdsa.PublicKey), digest[:], resp[1+phoneSPKILength:len(resp)-2]) {
		return nil, errors.New("invalid phone signature")
	}
	return append([]byte(nil), der...), nil
}

// readPhoneProof distinguishes a phone application from a legacy ISO-DEP
// card. Only an explicit SELECT success starts the phone authentication path.
func (s *Service) readPhoneProof(tag hal.Tag) (bool, []byte, error) {
	if tag.RFProtocol != hal.RFProtocolISODEP {
		return false, nil, nil
	}
	selectAPDU := append([]byte{0x00, 0xA4, 0x04, 0x00, byte(len(phoneAID))}, phoneAID...)
	selectAPDU = append(selectAPDU, 0x00)
	resp, err := s.nfc.ExchangeAPDU(selectAPDU)
	if err != nil {
		return true, nil, err // do not turn a failed exchange into UID access
	}
	if len(resp) != 2 || resp[0] != 0x90 || resp[1] != 0 {
		return false, nil, nil
	}
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		return true, nil, err
	}
	apdu := append([]byte{0x80, 0x10, 0x00, 0x00, 0x20}, challenge...)
	resp, err = s.nfc.ExchangeAPDU(apdu)
	if err != nil {
		return true, nil, err
	}
	der, err := verifyPhoneResponse(resp, challenge)
	return true, der, err
}

func (s *Service) handlePhone(der []byte) {
	id := phoneFingerprint(der)
	if s.masterTeachInMode || s.masterBootstrapMode {
		s.logger.Info("Phone credential cannot become a master")
		s.flashLED(s.rgbLed.Red, flashDuration)
		return
	}
	if s.learnMode {
		if s.phones.has(der) {
			s.publishEvent("phone-duplicate:" + id)
			s.flashLED(s.rgbLed.Red, flashDuration)
			return
		}
		for _, pending := range s.newPhones {
			if phoneFingerprint(pending) == id {
				s.flashLED(s.rgbLed.Red, flashDuration)
				return
			}
		}
		s.newPhones = append(s.newPhones, der)
		s.publishEvent("phone-learned:" + id)
		return
	}
	if !s.phones.has(der) {
		s.logger.Info("Unregistered phone credential", "fingerprint", id)
		s.flashLED(s.rgbLed.Red, flashDuration)
		return
	}
	s.grantAccess("PHONE-" + id)
}
