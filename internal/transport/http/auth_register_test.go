package httpapi

import "testing"

// The return parameter is attacker-supplied and ends up in a redirect. Without
// this it is an open redirect: a link that sends someone through a genuine
// Google sign-in and then out to somewhere else entirely, with the portal's
// name on the journey.
func TestReturnPathCannotLeaveThePortal(t *testing.T) {
	for _, hostile := range []string{
		"https://evil.example/steal",
		"//evil.example/steal",
		"http://evil.example",
		"/\\evil.example",
		"/vms\r\nSet-Cookie: x=1",
		"",
		"vms",
	} {
		if got := safeReturnPath(hostile); got != "/" {
			t.Errorf("safeReturnPath(%q) = %q, want /", hostile, got)
		}
	}
	for _, ok := range []string{"/", "/vms", "/vms/123?tab=performance"} {
		if got := safeReturnPath(ok); got != ok {
			t.Errorf("safeReturnPath(%q) = %q, want it unchanged", ok, got)
		}
	}
}
