package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// What the transport owes the second factor (AUTH-04): a challenged login must
// not look like a failure, must hand out nothing that resembles a session, and
// the two ways a code can be refused must be distinguishable — one means try
// again, the other means start over.

type fakeMFA struct {
	begin    command.EnrollTOTPOutput
	beginErr error

	confirmErr error
	confirmGot string

	disableErr error
	disableGot string

	verifyOut command.LoginOutput
	verifyErr error
	verifyGot command.VerifyMFAInput
}

func (f *fakeMFA) Begin(context.Context, command.Actor) (command.EnrollTOTPOutput, error) {
	return f.begin, f.beginErr
}

func (f *fakeMFA) Confirm(_ context.Context, _ command.Actor, code string) error {
	f.confirmGot = code
	return f.confirmErr
}

func (f *fakeMFA) Disable(_ context.Context, _ command.Actor, password string) error {
	f.disableGot = password
	return f.disableErr
}

func (f *fakeMFA) Verify(_ context.Context, in command.VerifyMFAInput) (command.LoginOutput, error) {
	f.verifyGot = in
	return f.verifyOut, f.verifyErr
}

func TestChallengedLoginIsNotAFailure(t *testing.T) {
	user := testUser()
	h := authServer(t, AuthDeps{
		Login: &fakeLogin{out: command.LoginOutput{
			MFARequired: true, MFAToken: "challenge-1", User: user,
		}},
		MFA: &fakeMFA{},
	})

	rec := postJSON(t, h, "/api/v1/auth/login",
		map[string]string{"username": "jsmith", "password": "correct horse"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a right password is not an error", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["mfa_required"] != true || body["mfa_token"] != "challenge-1" {
		t.Fatalf("body = %v, want a challenge", body)
	}
	// The half-finished sign-in must carry nothing usable.
	if _, ok := body["access_token"]; ok {
		t.Error("a challenged login returned an access token")
	}
	if findCookie(rec, refreshCookieName) != nil {
		t.Error("a challenged login set the refresh cookie")
	}
}

func TestVerifyMFACompletesTheSignIn(t *testing.T) {
	user := testUser()
	mfa := &fakeMFA{verifyOut: command.LoginOutput{
		AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresIn: 900_000_000_000, User: user,
	}}
	h := authServer(t, AuthDeps{Login: &fakeLogin{}, MFA: mfa})

	rec := postJSON(t, h, "/api/v1/auth/mfa",
		map[string]string{"mfa_token": "challenge-1", "code": "123456"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if mfa.verifyGot.ChallengeID != "challenge-1" || mfa.verifyGot.Code != "123456" {
		t.Errorf("command received %+v", mfa.verifyGot)
	}
	body := decodeBody(t, rec)
	if body["access_token"] != "access-1" {
		t.Errorf("body = %v, want the access token", body)
	}
	// Same shape as a password-only success, cookie included, so the browser
	// has one code path for "signed in".
	cookie := findCookie(rec, refreshCookieName)
	if cookie == nil || cookie.Value != "refresh-1" {
		t.Fatalf("refresh cookie = %v", cookie)
	}
	if !cookie.HttpOnly {
		t.Error("the refresh cookie is readable by script")
	}
}

func TestWrongCodeAndDeadChallengeAreDifferentAnswers(t *testing.T) {
	// The browser does different things with these: one keeps the code form
	// open, the other sends the operator back to the password.
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"wrong code", identity.ErrInvalidTOTPCode, "auth.invalid_code"},
		{"dead challenge", identity.ErrMFAChallengeNotFound, "auth.mfa_challenge_expired"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := authServer(t, AuthDeps{Login: &fakeLogin{}, MFA: &fakeMFA{verifyErr: c.err}})
			rec := postJSON(t, h, "/api/v1/auth/mfa",
				map[string]string{"mfa_token": "challenge-1", "code": "000000"})

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := decodeBody(t, rec)["code"]; got != c.want {
				t.Errorf("problem code = %v, want %s", got, c.want)
			}
		})
	}
}

func TestVerifyMFAWantsBothHalves(t *testing.T) {
	h := authServer(t, AuthDeps{Login: &fakeLogin{}, MFA: &fakeMFA{}})
	for _, body := range []map[string]string{
		{"code": "123456"},
		{"mfa_token": "challenge-1"},
		{},
	} {
		rec := postJSON(t, h, "/api/v1/auth/mfa", body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%v: status = %d, want 422", body, rec.Code)
		}
	}
}

func TestEnrolmentReturnsWhatAQRCodeNeeds(t *testing.T) {
	mfa := &fakeMFA{begin: command.EnrollTOTPOutput{
		Secret:     "JBSWY3DPEHPK3PXP",
		OTPAuthURL: "otpauth://totp/ProxUI:jsmith?secret=JBSWY3DPEHPK3PXP&issuer=ProxUI",
	}}
	h := authedServer(t, AuthDeps{Login: &fakeLogin{}, MFA: mfa})

	rec := postAuthed(t, h, http.MethodPost, "/api/v1/auth/me/totp", map[string]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["secret"] != "JBSWY3DPEHPK3PXP" {
		t.Errorf("secret = %v", body["secret"])
	}
	if body["otpauth_url"] == "" {
		t.Error("no otpauth url to draw a QR code from")
	}
}

func TestConfirmAndDisablePassTheirInputThrough(t *testing.T) {
	mfa := &fakeMFA{}
	h := authedServer(t, AuthDeps{Login: &fakeLogin{}, MFA: mfa})

	if rec := postAuthed(t, h, http.MethodPost, "/api/v1/auth/me/totp/confirm",
		map[string]string{"code": "654321"}); rec.Code != http.StatusNoContent {
		t.Fatalf("confirm status = %d, want 204", rec.Code)
	}
	if mfa.confirmGot != "654321" {
		t.Errorf("confirm received %q", mfa.confirmGot)
	}

	if rec := postAuthed(t, h, http.MethodDelete, "/api/v1/auth/me/totp",
		map[string]string{"password": "correct horse"}); rec.Code != http.StatusNoContent {
		t.Fatalf("disable status = %d, want 204", rec.Code)
	}
	if mfa.disableGot != "correct horse" {
		t.Errorf("disable received %q", mfa.disableGot)
	}
}

func TestEnrolmentRefusalsAreLegible(t *testing.T) {
	h := authedServer(t, AuthDeps{
		Login: &fakeLogin{},
		MFA:   &fakeMFA{confirmErr: identity.ErrInvalidTOTPCode},
	})
	rec := postAuthed(t, h, http.MethodPost, "/api/v1/auth/me/totp/confirm",
		map[string]string{"code": "000000"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := decodeBody(t, rec)["code"]; got != "auth.invalid_code" {
		t.Errorf("problem code = %v", got)
	}

	h = authedServer(t, AuthDeps{
		Login: &fakeLogin{},
		MFA:   &fakeMFA{beginErr: identity.ErrTOTPAlreadyEnabled},
	})
	rec = postAuthed(t, h, http.MethodPost, "/api/v1/auth/me/totp", map[string]string{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if got := decodeBody(t, rec)["code"]; got != "auth.totp_already_enabled" {
		t.Errorf("problem code = %v", got)
	}
}

// --- helpers -------------------------------------------------------------

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := map[string]any{}
	if rec.Body.Len() == 0 {
		return body
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body
}

// authedServer wires a server whose bearer token resolves to a signed-in
// operator, for the enrolment routes that need one.
func authedServer(t *testing.T, deps AuthDeps) http.Handler {
	t.Helper()
	user := testUser()
	claims := &crypto.Claims{Role: string(user.Role), SessionID: uuid.NewString()}
	claims.Subject = user.ID.String()

	deps.Tokens = &fakeTokenParser{claims: claims}
	deps.Sessions = &fakeSessionChecker{active: true}
	deps.Users = &fakeUserLoader{user: user}
	return authServer(t, deps)
}

func postAuthed(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
