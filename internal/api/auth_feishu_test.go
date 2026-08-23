package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/auth"
	"oncall-agent/internal/store"
)

type fakeAuthStore struct{}

func (fakeAuthStore) CreateWebSession(_ context.Context, session store.WebSession) (store.WebSession, error) {
	return session, nil
}
func (fakeAuthStore) GetWebSessionByTokenHash(context.Context, string, time.Time) (store.WebSession, error) {
	return store.WebSession{}, store.ErrWebSessionNotFound
}
func (fakeAuthStore) RevokeWebSession(context.Context, string, time.Time) error { return nil }
func (fakeAuthStore) CreateWebOAuthState(_ context.Context, state store.WebOAuthState) (store.WebOAuthState, error) {
	return state, nil
}
func (fakeAuthStore) ConsumeWebOAuthState(context.Context, string, time.Time) (store.WebOAuthState, error) {
	return store.WebOAuthState{}, store.ErrWebOAuthStateNotFound
}

type memoryOAuthStore struct {
	fakeAuthStore
	state store.WebOAuthState
}

func (s *memoryOAuthStore) CreateWebOAuthState(_ context.Context, state store.WebOAuthState) (store.WebOAuthState, error) {
	s.state = state
	return state, nil
}

func (s *memoryOAuthStore) ConsumeWebOAuthState(_ context.Context, hash string, _ time.Time) (store.WebOAuthState, error) {
	if hash != s.state.StateHash {
		return store.WebOAuthState{}, store.ErrWebOAuthStateNotFound
	}
	return s.state, nil
}

func TestOAuthCallbackRequiresInitiatingBrowserNonce(t *testing.T) {
	store := &memoryOAuthStore{}
	handler := NewFeishuAuthAPI(store, FeishuOAuthConfig{
		AppID: "cli_test", AppSecret: "secret", SessionSecret: strings.Repeat("x", 32),
		RedirectURI: "https://console.example.invalid/auth/feishu/callback",
	})
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/auth/feishu/start", nil))
	if start.Code != http.StatusFound {
		t.Fatalf("start = %d", start.Code)
	}
	location, err := start.Result().Location()
	if err != nil {
		t.Fatalf("location: %v", err)
	}
	state := location.Query().Get("state")
	callback := httptest.NewRecorder()
	handler.ServeHTTP(callback, httptest.NewRequest(http.MethodGet, "/auth/feishu/callback?code=attacker-code&state="+state, nil))
	if callback.Code != http.StatusBadRequest {
		t.Fatalf("callback without nonce = %d", callback.Code)
	}
}

func TestSessionUnauthorizedWithoutCookie(t *testing.T) {
	handler := NewFeishuAuthAPI(fakeAuthStore{}, FeishuOAuthConfig{
		AppID: "cli_test", AppSecret: "secret", SessionSecret: strings.Repeat("x", 32),
	})
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/v1/session", nil))
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("session = %d body=%s", resp.Code, resp.Body.String())
	}
}

// Trusted mode replaces the login step: an anonymous browser must come back with
// a usable session instead of a 401, and that identity must be able to approve.
func TestSessionMintsTrustedSessionWithoutCookie(t *testing.T) {
	handler := NewFeishuAuthAPI(fakeAuthStore{}, FeishuOAuthConfig{
		SessionSecret: strings.Repeat("x", 32), TrustedOperator: "oncall",
	})
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/v1/session", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("session = %d body=%s", resp.Code, resp.Body.String())
	}
	var body struct {
		Actor struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"actor"`
		CSRFToken  string `json:"csrf_token"`
		CanOperate bool   `json:"can_operate"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if body.Actor.ID != auth.TrustedActorPrefix+"oncall" || body.Actor.Name != "oncall" {
		t.Fatalf("actor = %+v", body.Actor)
	}
	// The minting request cannot carry the cookie being set, so the raw CSRF token
	// has to travel in the body or the console can never issue a write.
	if body.CSRFToken == "" {
		t.Fatal("csrf_token is empty")
	}
	if !body.CanOperate {
		t.Fatal("can_operate = false, trusted operator must be allowed to approve")
	}
	cookies := resp.Result().Cookies()
	names := make(map[string]bool, len(cookies))
	for _, cookie := range cookies {
		names[cookie.Name] = true
	}
	if !names[auth.SessionCookieName] || !names[auth.CSRFCookieName] {
		t.Fatalf("cookies = %v", names)
	}
}

// Trusted mode must not require OAuth credentials at startup; OAuth-only config
// stays mandatory when there is no trusted operator.
func TestStrictConstructorSkipsOAuthCredentialsInTrustedMode(t *testing.T) {
	if _, err := NewFeishuAuthAPIWithError(fakeAuthStore{}, FeishuOAuthConfig{
		SessionSecret: strings.Repeat("x", 32), TrustedOperator: "oncall",
	}); err != nil {
		t.Fatalf("trusted construction error = %v", err)
	}
	if _, err := NewFeishuAuthAPIWithError(fakeAuthStore{}, FeishuOAuthConfig{
		SessionSecret: strings.Repeat("x", 32),
	}); err == nil {
		t.Fatal("OAuth mode without credentials must fail at startup")
	}
}

func TestLogoutClearsCookies(t *testing.T) {
	handler := NewFeishuAuthAPI(fakeAuthStore{}, FeishuOAuthConfig{
		AppID: "cli_test", AppSecret: "secret", SessionSecret: strings.Repeat("x", 32),
	})
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
	if resp.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", resp.Code)
	}
}
