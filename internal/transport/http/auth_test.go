package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// --- fakes -------------------------------------------------------------

type fakeLogin struct {
	out command.LoginOutput
	err error
	got command.LoginInput
}

func (f *fakeLogin) Handle(_ context.Context, in command.LoginInput) (command.LoginOutput, error) {
	f.got = in
	return f.out, f.err
}

type fakeRefresh struct {
	out command.LoginOutput
	err error
	got command.RefreshInput
}

func (f *fakeRefresh) Handle(_ context.Context, in command.RefreshInput) (command.LoginOutput, error) {
	f.got = in
	return f.out, f.err
}

type fakeLogout struct {
	err  error
	got  command.LogoutInput
	call int
}

func (f *fakeLogout) Handle(_ context.Context, in command.LogoutInput) error {
	f.got = in
	f.call++
	return f.err
}

type fakeUserLoader struct {
	user *identity.User
	err  error
}

func (f *fakeUserLoader) GetByID(context.Context, uuid.UUID) (*identity.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}

type fakeTokenParser struct {
	claims *crypto.Claims
	err    error
}

func (f *fakeTokenParser) Parse(string) (*crypto.Claims, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.claims, nil
}

type fakeSessionChecker struct {
	active bool
	err    error
}

func (f *fakeSessionChecker) IsSessionActive(context.Context, uuid.UUID) (bool, error) {
	return f.active, f.err
}

// --- helpers -----------------------------------------------------------

func testUser() *identity.User {
	return &identity.User{
		ID: uuid.New(), Username: "jsmith", Email: "jsmith@example.test",
		DisplayName: "J Smith", Role: identity.RoleOperator, IsActive: true,
	}
}

func authServer(t *testing.T, deps AuthDeps) http.Handler {
	t.Helper()
	return NewServer(ServerConfig{
		Log: zerolog.New(io.Discard), Version: "test",
		Auth: deps, SecureCookies: true,
		Clock: func() time.Time { return time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC) },
	}).Routes()
}

func postJSON(t *testing.T, h http.Handler, path string, body any, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// --- tests -------------------------------------------------------------

func TestLoginReturnsTokenAndHardenedCookie(t *testing.T) {
	user := testUser()
	login := &fakeLogin{out: command.LoginOutput{
		AccessToken: "jwt-value", ExpiresIn: 15 * time.Minute,
		RefreshToken: "refresh-value", User: user,
	}}
	h := authServer(t, AuthDeps{Login: login})

	rec := postJSON(t, h, "/api/v1/auth/login", loginRequest{Username: "jsmith", Password: "correct horse battery staple"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	var body tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AccessToken != "jwt-value" || body.TokenType != "Bearer" || body.ExpiresIn != 900 {
		t.Errorf("body = %+v, want jwt-value/Bearer/900", body)
	}

	c := findCookie(rec, refreshCookieName)
	if c == nil {
		t.Fatal("refresh cookie was not set")
	}
	if c.Value != "refresh-value" {
		t.Errorf("cookie value = %q, want refresh-value", c.Value)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie hardening missing: HttpOnly=%v Secure=%v SameSite=%v", c.HttpOnly, c.Secure, c.SameSite)
	}
	if c.Path != refreshCookiePath {
		t.Errorf("cookie path = %q, want %q so it never rides on ordinary API calls", c.Path, refreshCookiePath)
	}

	// The refresh token must never appear in the response body.
	if bytes.Contains(rec.Body.Bytes(), []byte("refresh-value")) {
		t.Error("refresh token leaked into the JSON body")
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"wrong password", identity.ErrInvalidCredentials, http.StatusUnauthorized, "auth.invalid_credentials"},
		{"inactive account", identity.ErrAccountInactive, http.StatusUnauthorized, "auth.invalid_credentials"},
		{"locked account", identity.ErrAccountLocked, http.StatusLocked, "auth.account_locked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := authServer(t, AuthDeps{Login: &fakeLogin{err: tt.err}})
			rec := postJSON(t, h, "/api/v1/auth/login", loginRequest{Username: "jsmith", Password: "x"})

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			var p Problem
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if p.Code != tt.code {
				t.Errorf("code = %q, want %q", p.Code, tt.code)
			}
			if findCookie(rec, refreshCookieName) != nil {
				t.Error("a failed login set a refresh cookie")
			}
		})
	}
}

func TestLoginValidatesInput(t *testing.T) {
	h := authServer(t, AuthDeps{Login: &fakeLogin{}})

	rec := postJSON(t, h, "/api/v1/auth/login", loginRequest{Username: "", Password: ""})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	var p Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if len(p.Fields) == 0 {
		t.Error("422 response carried no field errors")
	}
}

func TestLoginPassesRequestMetadataToAudit(t *testing.T) {
	login := &fakeLogin{out: command.LoginOutput{User: testUser()}}
	h := authServer(t, AuthDeps{Login: login})

	postJSON(t, h, "/api/v1/auth/login",
		loginRequest{Username: "jsmith", Password: "correct horse battery staple"},
		func(r *http.Request) {
			r.RemoteAddr = "203.0.113.9:4444"
			r.Header.Set("User-Agent", "test-agent/1.0")
		})

	if login.got.IP == "" || login.got.UserAgent != "test-agent/1.0" {
		t.Errorf("audit metadata not forwarded: %+v", login.got)
	}
	if login.got.RequestID == "" {
		t.Error("request ID not forwarded to the command")
	}
}

func TestRefreshRequiresCookie(t *testing.T) {
	h := authServer(t, AuthDeps{Refresh: &fakeRefresh{}})

	rec := postJSON(t, h, "/api/v1/auth/refresh", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRefreshRotatesCookie(t *testing.T) {
	refresh := &fakeRefresh{out: command.LoginOutput{
		AccessToken: "new-jwt", ExpiresIn: 15 * time.Minute,
		RefreshToken: "rotated-refresh", User: testUser(),
	}}
	h := authServer(t, AuthDeps{Refresh: refresh})

	rec := postJSON(t, h, "/api/v1/auth/refresh", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "old-refresh"})
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if refresh.got.RefreshToken != "old-refresh" {
		t.Errorf("command received %q, want the cookie value", refresh.got.RefreshToken)
	}
	if c := findCookie(rec, refreshCookieName); c == nil || c.Value != "rotated-refresh" {
		t.Error("rotated refresh token was not written back to the cookie")
	}
}

func TestRefreshFailureClearsCookie(t *testing.T) {
	h := authServer(t, AuthDeps{Refresh: &fakeRefresh{err: identity.ErrRefreshTokenReuse}})

	rec := postJSON(t, h, "/api/v1/auth/refresh", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "stolen"})
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	c := findCookie(rec, refreshCookieName)
	if c == nil || c.MaxAge >= 0 {
		t.Error("rejected refresh did not clear the cookie")
	}
}

func TestLogoutClearsCookieAndRevokes(t *testing.T) {
	logout := &fakeLogout{}
	h := authServer(t, AuthDeps{Logout: logout})

	rec := postJSON(t, h, "/api/v1/auth/logout", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "session-token"})
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if logout.got.RefreshToken != "session-token" {
		t.Errorf("logout received %q, want the cookie value", logout.got.RefreshToken)
	}
	if c := findCookie(rec, refreshCookieName); c == nil || c.MaxAge >= 0 {
		t.Error("logout did not clear the refresh cookie")
	}
}

func TestProtectedRoutesRequireValidToken(t *testing.T) {
	user := testUser()
	validClaims := &crypto.Claims{Role: string(user.Role), SessionID: uuid.NewString()}
	validClaims.Subject = user.ID.String()

	tests := []struct {
		name     string
		deps     AuthDeps
		header   string
		wantCode int
		wantErr  string
	}{
		{
			name:     "no token",
			deps:     AuthDeps{Tokens: &fakeTokenParser{claims: validClaims}, Sessions: &fakeSessionChecker{active: true}, Users: &fakeUserLoader{user: user}},
			wantCode: http.StatusUnauthorized, wantErr: "auth.missing_token",
		},
		{
			name:     "invalid token",
			deps:     AuthDeps{Tokens: &fakeTokenParser{err: crypto.ErrInvalidToken}, Sessions: &fakeSessionChecker{active: true}, Users: &fakeUserLoader{user: user}},
			header:   "Bearer garbage",
			wantCode: http.StatusUnauthorized, wantErr: "auth.invalid_token",
		},
		{
			name:     "revoked session",
			deps:     AuthDeps{Tokens: &fakeTokenParser{claims: validClaims}, Sessions: &fakeSessionChecker{active: false}, Users: &fakeUserLoader{user: user}},
			header:   "Bearer good",
			wantCode: http.StatusUnauthorized, wantErr: "auth.session_revoked",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			authServer(t, tt.deps).ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			var p Problem
			_ = json.Unmarshal(rec.Body.Bytes(), &p)
			if p.Code != tt.wantErr {
				t.Errorf("code = %q, want %q", p.Code, tt.wantErr)
			}
		})
	}
}

func TestMeReturnsCurrentUser(t *testing.T) {
	user := testUser()
	claims := &crypto.Claims{Role: string(user.Role), SessionID: uuid.NewString()}
	claims.Subject = user.ID.String()

	h := authServer(t, AuthDeps{
		Tokens:   &fakeTokenParser{claims: claims},
		Sessions: &fakeSessionChecker{active: true},
		Users:    &fakeUserLoader{user: user},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var body meResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Username != user.Username || body.Role != string(identity.RoleOperator) {
		t.Errorf("body = %+v, want jsmith/operator", body)
	}
}

func TestMeHandlesDeletedAccount(t *testing.T) {
	claims := &crypto.Claims{Role: "operator", SessionID: uuid.NewString()}
	claims.Subject = uuid.NewString()

	h := authServer(t, AuthDeps{
		Tokens:   &fakeTokenParser{claims: claims},
		Sessions: &fakeSessionChecker{active: true},
		Users:    &fakeUserLoader{err: ports.ErrNotFound},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestRequireRoleGatesByRole(t *testing.T) {
	handler := RequireRole(identity.RoleAdmin)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		role identity.Role
		want int
	}{
		{identity.RoleAdmin, http.StatusOK},
		{identity.RoleOperator, http.StatusForbidden},
		{identity.RoleReadOnly, http.StatusForbidden},
		{identity.RoleAuditor, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req = req.WithContext(WithPrincipal(req.Context(), Principal{UserID: uuid.New(), Role: tt.role}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("role %s: status = %d, want %d", tt.role, rec.Code, tt.want)
			}
		})
	}
}

func TestRequireRoleRejectsAnonymous(t *testing.T) {
	handler := RequireRole(identity.RoleAdmin)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestMalformedJSONIsRejected(t *testing.T) {
	h := authServer(t, AuthDeps{Login: &fakeLogin{}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader([]byte(`{"username":`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestUnknownJSONFieldsAreRejected(t *testing.T) {
	h := authServer(t, AuthDeps{Login: &fakeLogin{}})

	body := `{"username":"jsmith","password":"correct horse battery staple","role":"admin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: unknown fields must not be silently ignored", rec.Code)
	}
}

var _ = io.Discard
