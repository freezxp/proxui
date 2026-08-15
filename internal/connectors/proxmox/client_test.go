package proxmox

// An internal test: classifyStatus is unexported, and the mapping from HTTP
// status to error class is exactly the thing worth pinning — the sync engine
// chooses retry, circuit-break or surface-to-admin from the class alone.

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/freezxp/proxui/internal/connector"
)

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

// A 5xx from Proxmox means it was reached, understood the request and would
// not perform it. Calling that "unreachable" sends an operator to debug a
// network that is fine, and — because ErrUnreachable is retryable — has the
// sync engine retry a call that can never succeed.
func TestServerErrorsAreRefusalsRatherThanUnreachable(t *testing.T) {
	err := classifyStatus("/nodes/pve/lxc/126/status/start",
		response(http.StatusInternalServerError, `{"data":null,"message":"VM 126 is already running"}`))

	if !errors.Is(err, connector.ErrRefused) {
		t.Errorf("class = %v, want ErrRefused", err)
	}
	if errors.Is(err, connector.ErrUnreachable) {
		t.Error("a platform that answered was classified as unreachable")
	}
	if connector.Retryable(err) {
		t.Error("a refusal was marked retryable; retrying cannot make it succeed")
	}
	// The platform's own words are the whole value of the message.
	if !strings.Contains(err.Error(), "VM 126 is already running") {
		t.Errorf("error = %q, want it to carry the platform's explanation", err)
	}
}

// Proxmox reports parameter faults in an `errors` map rather than `message`.
func TestRefusalCarriesTheErrorsMap(t *testing.T) {
	err := classifyStatus("/nodes/pve/qemu/999/status/start",
		response(http.StatusInternalServerError,
			`{"data":null,"errors":{"vmid":"no such VM","node":"unknown node"}}`))

	// Sorted, because an error that reads differently each time it is logged
	// is one nobody can search for.
	if !strings.Contains(err.Error(), "node: unknown node; vmid: no such VM") {
		t.Errorf("error = %q, want the map rendered in a stable order", err)
	}
}

// An empty or unparseable body must not stop the status being classified.
func TestRefusalSurvivesAnUnhelpfulBody(t *testing.T) {
	for _, body := range []string{"", "not json at all", `{"data":null}`} {
		err := classifyStatus("/nodes/pve/qemu/1/status/stop",
			response(http.StatusInternalServerError, body))
		if !errors.Is(err, connector.ErrRefused) {
			t.Errorf("body %q: class = %v, want ErrRefused", body, err)
		}
	}
}

// 401 and 403 keep their own classes: those name a credential problem, which
// is a different person's job from a refused operation.
func TestCredentialFailuresAreStillDistinct(t *testing.T) {
	cases := map[int]error{
		http.StatusUnauthorized:    connector.ErrAuth,
		http.StatusForbidden:       connector.ErrPermission,
		http.StatusTooManyRequests: connector.ErrThrottled,
		http.StatusNotFound:        connector.ErrNotSupported,
	}
	for status, want := range cases {
		if err := classifyStatus("/nodes/pve/qemu/1/status/start", response(status, "{}")); !errors.Is(err, want) {
			t.Errorf("HTTP %d classified as %v, want %v", status, err, want)
		}
	}
}

func TestSuccessIsNotAnError(t *testing.T) {
	if err := classifyStatus("/version", response(http.StatusOK, "{}")); err != nil {
		t.Errorf("200 returned %v", err)
	}
}
