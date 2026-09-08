package keycard

import "testing"

// command-result prose is a compatibility surface: a change to any string here
// is a change to the wire.
func TestErrorProse_PreservesLegacyWording(t *testing.T) {
	want := map[string]string{
		codeEmptyUID:          "error:empty uid",
		codeAlreadyAuthorized: "error:already authorized",
		codeNotFound:          "error:not found",
		codeLastCredential:    "error:cannot remove last authorized card",
		codeUnknownCommand:    "error:unknown command",
	}
	for code, expected := range want {
		if got := prose(code); got != expected {
			t.Errorf("prose(%q) = %q, want %q", code, got, expected)
		}
	}
}

// A code with no prose entry must still produce a result that reads as an
// error, or a client checking for the "error:" prefix would call it a success.
func TestProse_UnknownCodeStillReadsAsError(t *testing.T) {
	if got := prose("something-new"); got != "error:something-new" {
		t.Errorf("prose fallback = %q", got)
	}
}

func TestLegacyModeProse(t *testing.T) {
	cases := []struct {
		command, mode, want string
	}{
		{"learn:start", "learn", "error:already in learn mode"},
		{"learn:start", "master-bootstrap", "error:in master learning mode"},
		{"learn:stop", "idle", "error:not in learn mode"},
		{"learn:stop", "master-teach-in", "error:not in learn mode"},
		{"learn:master:start", "master-teach-in", "error:already in master teach-in"},
		{"learn:master:start", "learn", "error:in learn mode"},
		{"learn:master:start", "master-bootstrap", "error:in master learning mode"},
		{"learn:master:stop", "idle", "error:not in master teach-in"},
	}
	for _, c := range cases {
		if got := legacyModeProse(c.command, c.mode); got != c.want {
			t.Errorf("legacyModeProse(%q, %q) = %q, want %q", c.command, c.mode, got, c.want)
		}
	}
}

func TestErrorCode(t *testing.T) {
	if _, err := NormalizeUID(""); errorCode(err) != codeEmptyUID {
		t.Errorf("empty uid mapped to %q", errorCode(err))
	}
	if _, err := NormalizeUID("NOTHEX"); errorCode(err) != codeBadUID {
		t.Errorf("malformed uid mapped to %q", errorCode(err))
	}
	if errorCode(ErrLastCredential) != codeLastCredential {
		t.Errorf("last credential mapped to %q", errorCode(ErrLastCredential))
	}
}
