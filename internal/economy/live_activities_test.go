package economy

import "testing"

// B41.136: the first of two approvals leaves an approval pending, and its Live Activity stays up; only a decided one
// ends, showing approved or declined.
func TestB41136_OnlyADecidedApprovalEndsItsLiveActivity(t *testing.T) {
	for status, want := range map[string]string{"approved": "approved", "denied": "declined", "pending": ""} {
		if got, ended := decidedActivityState(status); got != want || ended != (want != "") {
			t.Errorf("decidedActivityState(%q) = %q, %v; want %q", status, got, ended, want)
		}
	}
}
