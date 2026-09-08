package keycard

import (
	"errors"
	"fmt"
	"strings"
)

// MasterDisabled records that no physical master is wanted. Unlike an empty
// master list, it does not re-arm bootstrap on the next start.
const MasterDisabled = "NONE"

// ErrInvalidUID is returned for anything that is not the sentinel and not a
// 1-10 byte hex string.
var ErrInvalidUID = errors.New("uid must be 1-10 bytes of hex")

// ErrEmptyUID wraps ErrInvalidUID, so errors.Is still answers for either.
var ErrEmptyUID = fmt.Errorf("%w: empty", ErrInvalidUID)

// NormalizeUID is the single entry point for every UID, whatever its source.
// A UID stored in any other shape can never match a tap.
func NormalizeUID(uid string) (string, error) {
	r := strings.NewReplacer(":", "", "-", "", " ", "", ".", "")
	uid = strings.ToUpper(strings.TrimSpace(r.Replace(uid)))

	if uid == "" {
		return "", ErrEmptyUID
	}

	if uid == MasterDisabled {
		return uid, nil
	}

	// 10 bytes is the widest an NCI notification can carry.
	if len(uid) < 2 || len(uid) > 20 || len(uid)%2 != 0 {
		return "", ErrInvalidUID
	}
	for _, c := range uid {
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return "", ErrInvalidUID
		}
	}
	return uid, nil
}
