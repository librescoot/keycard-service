package keycard

import "strings"

// ISO/IEC 14443-3 reserves a first UID byte of 0x08 for random IDs. A physical
// card never presents one, but a phone that does not answer as a phone key
// usually does, with a different value on every tap.
func isRandomUID(uid string) bool {
	return len(uid) == 8 && strings.EqualFold(uid[:2], "08")
}

// acceptEnrollmentUID reports whether an enrollment tap may be used. A random
// UID is discarded with a warning event the first time. Presenting the same UID
// again accepts it, so a non-standard card with a fixed random-looking UID can
// still be enrolled while an ever-changing phone ID never can.
func (s *Service) acceptEnrollmentUID(uid string) bool {
	uid = strings.ToUpper(uid)
	if !isRandomUID(uid) {
		return true
	}
	if s.randomUIDPending == uid {
		s.randomUIDPending = ""
		return true
	}
	s.randomUIDPending = uid
	s.logger.Warn("Random UID discarded; present the same ID again to enroll it", "uid", uid)
	s.flashLED(s.rgbLed.Red, flashDuration)
	s.publishEvent("card-random-id:" + uid)
	return false
}
