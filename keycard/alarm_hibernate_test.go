package keycard

import (
	"strings"
	"testing"
)

func TestAlarmHibernateSettingGate(t *testing.T) {
	for _, general := range []string{"", "false", "true"} {
		for _, hibernate := range []string{"", "false", "true", "invalid"} {
			for _, power := range []string{"running", "suspending", "suspending-imminent", "hibernating-for-pending", "hibernating-imminent", "hibernating-for-imminent", "hibernating", "hibernating-manual", "hibernating-timer", "hibernating-for"} {
				want := general == "true"
				if strings.HasPrefix(power, "hibernating") && !strings.HasSuffix(power, "-pending") {
					want = want && (hibernate == "" || hibernate == "true")
				}
				got := alarmIndicatorEnabled(general, hibernate, "armed", "stand-by", power)
				if got != want {
					t.Errorf("general=%q hibernate=%q power=%q: got %v, want %v", general, hibernate, power, got, want)
				}
			}
		}
	}
}
