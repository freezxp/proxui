package setting_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/freezxp/proxui/internal/domain/setting"
)

// An unset portal name must reach the browser as empty rather than as a
// stand-in, because the browser is the only party that knows which address
// was actually typed.
func TestUnsetPortalNameStaysEmpty(t *testing.T) {
	public := setting.PublicValues(map[string]json.RawMessage{})
	if got, ok := public["branding.portal_name"]; !ok || got != "" {
		t.Errorf("portal name = %q (present=%v), want an empty string", got, ok)
	}
}

func TestStoredPortalNameIsReturned(t *testing.T) {
	public := setting.PublicValues(map[string]json.RawMessage{
		"branding.portal_name": json.RawMessage(`"exstudios.vm"`),
	})
	if public["branding.portal_name"] != "exstudios.vm" {
		t.Errorf("portal name = %q, want exstudios.vm", public["branding.portal_name"])
	}
}

// Branding is the only group the sign-in page may read unauthenticated.
// Anything else appearing here would be a leak.
func TestOnlyBrandingIsPublic(t *testing.T) {
	stored := map[string]json.RawMessage{
		"console.idle_timeout_s": json.RawMessage(`900`),
		"retention.audit_days":   json.RawMessage(`30`),
	}
	for key := range setting.PublicValues(stored) {
		if len(key) < 9 || key[:9] != "branding." {
			t.Errorf("%q is exposed without authentication", key)
		}
	}
}

func TestResolveMarksOnlyChangedValues(t *testing.T) {
	stored := map[string]json.RawMessage{
		"console.idle_timeout_s": json.RawMessage(`900`),
		"branding.portal_name":   json.RawMessage(`"exstudios.vm"`),
	}
	for _, v := range setting.Resolve(stored) {
		switch v.Key {
		case "console.idle_timeout_s":
			if v.Value != 900 || !v.Modified {
				t.Errorf("idle timeout = %d modified=%v, want 900 and modified", v.Value, v.Modified)
			}
		case "branding.portal_name":
			if v.Text != "exstudios.vm" || !v.Modified {
				t.Errorf("portal name = %q modified=%v", v.Text, v.Modified)
			}
		case "retention.audit_days":
			if v.Modified {
				t.Error("an untouched setting was reported as modified")
			}
		}
	}
}

// A value stored under an old kind must not take the page down with it.
func TestResolveIgnoresUndecodableValues(t *testing.T) {
	stored := map[string]json.RawMessage{
		"console.idle_timeout_s": json.RawMessage(`"not a number"`),
	}
	for _, v := range setting.Resolve(stored) {
		if v.Key != "console.idle_timeout_s" {
			continue
		}
		if v.Value != v.Default {
			t.Errorf("value = %d, want the default %d", v.Value, v.Default)
		}
	}
}

func TestLogoRejectsAnythingTheBrowserCannotRender(t *testing.T) {
	def, ok := setting.Lookup("branding.logo")
	if !ok {
		t.Fatal("the logo setting is missing from the catalogue")
	}
	for _, value := range []string{
		"https://example.com/logo.png", // blocked by the CSP; refuse it here
		"http://10.0.0.1/logo.png",     // same, and an internal probe besides
		"data:image/svg+xml,<svg/>",    // not base64
		"javascript:alert(1)",          // not an image at all
	} {
		if err := def.ValidateText(value); err == nil {
			t.Errorf("%q was accepted as a logo", value)
		}
	}
	for _, value := range []string{
		"",
		"/brand/exstudios-mark.svg",
		"data:image/png;base64,iVBORw0KGgo=",
	} {
		if err := def.ValidateText(value); err != nil {
			t.Errorf("%q was rejected: %v", value, err)
		}
	}
}

func TestKindMismatchIsRefused(t *testing.T) {
	name, _ := setting.Lookup("branding.portal_name")
	if err := name.ValidateNumber(5); !errors.Is(err, setting.ErrInvalidValue) {
		t.Errorf("a number into a text setting gave %v", err)
	}
	timeout, _ := setting.Lookup("console.idle_timeout_s")
	if err := timeout.ValidateText("soon"); !errors.Is(err, setting.ErrInvalidValue) {
		t.Errorf("text into a numeric setting gave %v", err)
	}
	if err := timeout.ValidateNumber(10); !errors.Is(err, setting.ErrOutOfRange) {
		t.Errorf("an out-of-range number gave %v", err)
	}
}
