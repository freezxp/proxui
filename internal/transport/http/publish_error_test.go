package httpapi

import (
	"errors"
	"strings"
	"testing"

	"github.com/freezxp/proxui/internal/edge"
)

// Publishing writes to two systems needing two permissions, granted on two
// different tabs of Cloudflare's token editor. Telling someone to add both
// when one is already there is advice that gets ignored, so the operation that
// actually failed has to decide the wording.
//
// The live account produced exactly this shape: DNS: Edit present, Cloudflare
// Tunnel: Edit absent, and a generic "you need both" message would have sent
// the reader looking at the half that was already correct.
func TestTheMissingPermissionMessageNamesTheOperationThatFailed(t *testing.T) {
	cases := []struct {
		op       string
		wants    string
		notWants string
	}{
		{"put_ingress", "Cloudflare Tunnel : Edit", "DNS : Edit"},
		{"create_dns", "DNS : Edit", "Cloudflare Tunnel : Edit"},
		{"delete_dns", "DNS : Edit", "Cloudflare Tunnel : Edit"},
	}
	for _, c := range cases {
		err := edge.Errorf(edge.ErrAuth, c.op, "the API token was rejected (HTTP 401)")
		got := missingWritePermission(err)

		if !strings.Contains(got, c.wants) {
			t.Errorf("op %s: message %q does not name %q", c.op, got, c.wants)
		}
		if strings.Contains(got, c.notWants) {
			t.Errorf("op %s: message names %q, which is not the one that failed", c.op, c.notWants)
		}
		// Every wording has to end the same way: whatever failed, the caller
		// needs to know the routing table was left alone.
		if !strings.Contains(got, "Nothing was changed") {
			t.Errorf("op %s: message %q does not say the change was not applied", c.op, got)
		}
	}
}

// An error that carries no operation still has to say something useful rather
// than nothing.
func TestTheMissingPermissionMessageFallsBackWithoutAnOperation(t *testing.T) {
	got := missingWritePermission(errors.New("something else entirely"))
	if !strings.Contains(got, "write permission") || !strings.Contains(got, "Nothing was changed") {
		t.Errorf("fallback message = %q", got)
	}
}
