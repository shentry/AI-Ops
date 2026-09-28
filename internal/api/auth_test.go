package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/config"
)

const testMachineToken = "secret"

var (
	testViewerToken   = strings.Repeat("v", 64)
	testOperatorToken = strings.Repeat("o", 64)
	testAdminToken    = strings.Repeat("a", 64)
)

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// testAuth has the machine token "secret" and one operator per role.
func testAuth(t *testing.T) *Auth {
	t.Helper()
	auth, err := NewAuth(testMachineToken, []config.OperatorConfig{
		{ID: "viewer", Name: "只读", Role: "viewer", TokenSHA256: tokenHash(testViewerToken)},
		{ID: "ops", Name: "值班", Role: "operator", TokenSHA256: tokenHash(testOperatorToken)},
		{ID: "admin", Name: "管理员", Role: "admin", TokenSHA256: tokenHash(testAdminToken)},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

// login exchanges a personal token for the session cookie, as the browser does.
func login(t *testing.T, auth *Auth, token string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(`{"token":"`+token+`"}`))
	req.Header.Set(csrfHeader, csrfValue)
	resp := httptest.NewRecorder()
	NewSessionAPI(auth).ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("login = %d %s", resp.Code, resp.Body.String())
	}
	for _, cookie := range resp.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	t.Fatal("login set no session cookie")
	return nil
}

func withBearer(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// Self-asserted identity is never identity: only the machine token, a
// configured personal token or a signed session resolves to an actor.
func TestAuthenticateResolvesOnlyServerProvenIdentities(t *testing.T) {
	auth := testAuth(t)
	anonymous := httptest.NewRequest(http.MethodGet, "/", nil)
	anonymous.Header.Set("X-Operator", "admin")
	for name, req := range map[string]*http.Request{
		"x-operator only": anonymous,
		"wrong bearer":    withBearer(httptest.NewRequest(http.MethodGet, "/", nil), "wrong"),
		"short token":     withBearer(httptest.NewRequest(http.MethodGet, "/", nil), "o"),
	} {
		if actor, ok := auth.Authenticate(req); ok {
			t.Fatalf("%s authenticated as %+v", name, actor)
		}
	}
	machine, ok := auth.Authenticate(withBearer(httptest.NewRequest(http.MethodGet, "/", nil), testMachineToken))
	if !ok || !machine.Machine || machine.ID != "machine" {
		t.Fatalf("machine = %+v %v", machine, ok)
	}
	operator, ok := auth.Authenticate(withBearer(httptest.NewRequest(http.MethodPost, "/", nil), testOperatorToken))
	if !ok || operator.Machine || operator.ID != "ops" || operator.Role != RoleOperator || operator.Source != "api" {
		t.Fatalf("operator = %+v %v", operator, ok)
	}
}

func TestRequireEnforcesRolesAndMachineScope(t *testing.T) {
	auth := testAuth(t)
	for _, tc := range []struct {
		token        string
		role         Role
		allowMachine bool
		want         int
	}{
		{"", RoleViewer, true, http.StatusUnauthorized},
		{testMachineToken, RoleViewer, true, http.StatusOK},
		{testMachineToken, RoleOperator, false, http.StatusForbidden},
		{testViewerToken, RoleOperator, false, http.StatusForbidden},
		{testOperatorToken, RoleOperator, false, http.StatusOK},
		{testOperatorToken, RoleAdmin, false, http.StatusForbidden},
		{testAdminToken, RoleOperator, false, http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if tc.token != "" {
			withBearer(req, tc.token)
		}
		resp := httptest.NewRecorder()
		if _, ok := auth.Require(resp, req, tc.role, tc.allowMachine); ok {
			resp.WriteHeader(http.StatusOK)
		}
		if resp.Code != tc.want {
			t.Fatalf("token=%.8s role=%s machine=%v: %d, want %d", tc.token, tc.role, tc.allowMachine, resp.Code, tc.want)
		}
	}
}

func TestSessionCookieLoginCSRFAndExpiry(t *testing.T) {
	auth := testAuth(t)
	sessions := NewSessionAPI(auth)

	noHeader := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(`{"token":"`+testOperatorToken+`"}`))
	resp := httptest.NewRecorder()
	sessions.ServeHTTP(resp, noHeader)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("login without CSRF header = %d", resp.Code)
	}
	wrong := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(`{"token":"`+strings.Repeat("x", 64)+`"}`))
	wrong.Header.Set(csrfHeader, csrfValue)
	resp = httptest.NewRecorder()
	sessions.ServeHTTP(resp, wrong)
	if resp.Code != http.StatusUnauthorized || len(resp.Result().Cookies()) != 0 {
		t.Fatalf("wrong token login = %d cookies=%v", resp.Code, resp.Result().Cookies())
	}

	cookie := login(t, auth, testOperatorToken)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || strings.Contains(cookie.Value, testOperatorToken) {
		t.Fatalf("cookie = %+v", cookie)
	}
	whoami := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	whoami.AddCookie(cookie)
	resp = httptest.NewRecorder()
	sessions.ServeHTTP(resp, whoami)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"id":"ops"`) || !strings.Contains(resp.Body.String(), `"role":"operator"`) {
		t.Fatalf("whoami = %d %s", resp.Code, resp.Body.String())
	}

	write := httptest.NewRequest(http.MethodPost, "/", nil)
	write.AddCookie(cookie)
	if _, ok := auth.Authenticate(write); ok {
		t.Fatal("cookie write without CSRF header authenticated")
	}
	write.Header.Set(csrfHeader, csrfValue)
	if actor, ok := auth.Authenticate(write); !ok || actor.ID != "ops" || actor.Source != "web" {
		t.Fatalf("cookie write = %+v %v", actor, ok)
	}

	tampered := httptest.NewRequest(http.MethodGet, "/", nil)
	tampered.AddCookie(&http.Cookie{Name: sessionCookie, Value: auth.signSession("admin", time.Now().Add(time.Hour)) + "x"})
	if _, ok := auth.Authenticate(tampered); ok {
		t.Fatal("tampered session accepted")
	}
	auth.now = func() time.Time { return time.Now().Add(sessionTTL + time.Minute) }
	expired := httptest.NewRequest(http.MethodGet, "/", nil)
	expired.AddCookie(cookie)
	if _, ok := auth.Authenticate(expired); ok {
		t.Fatal("expired session accepted")
	}
	auth.now = time.Now
	delete(auth.byID, "ops")
	revoked := httptest.NewRequest(http.MethodGet, "/", nil)
	revoked.AddCookie(cookie)
	if _, ok := auth.Authenticate(revoked); ok {
		t.Fatal("session of a removed operator accepted")
	}

	logout := httptest.NewRecorder()
	sessions.ServeHTTP(logout, httptest.NewRequest(http.MethodDelete, "/api/v1/session", nil))
	if logout.Code != http.StatusNoContent || len(logout.Result().Cookies()) != 1 || logout.Result().Cookies()[0].MaxAge >= 0 && logout.Result().Cookies()[0].Value != "" {
		t.Fatalf("logout = %d %v", logout.Code, logout.Result().Cookies())
	}
}
