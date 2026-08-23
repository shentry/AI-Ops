package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/auth"
	"oncall-agent/internal/store"
)

const (
	defaultFeishuAuthorizeURL = "https://accounts.feishu.cn/open-apis/authen/v1/authorize"
	defaultFeishuTokenURL     = "https://accounts.feishu.cn/oauth/v3/token"
	defaultFeishuUserInfoURL  = "https://open.feishu.cn/open-apis/authen/v1/user_info"
	defaultOAuthStateTTL      = 10 * time.Minute
	oauthNonceCookieName      = "oncall_oauth_nonce"
)

// FeishuOAuthConfig is the narrow configuration needed by the Web OAuth
// boundary. AppID/AppSecret are aliases for ClientID/ClientSecret because
// Feishu's application terminology and OAuth terminology differ.
type FeishuOAuthConfig struct {
	AppID        string
	AppSecret    string
	ClientID     string
	ClientSecret string

	BaseURL     string
	RedirectURI string
	Scope       string

	AuthorizeURL string
	TokenURL     string
	UserInfoURL  string
	SuccessPath  string
	StateTTL     time.Duration

	Session auth.SessionConfig
	// These fields are copied into Session when Session is left at its zero
	// value, making direct config wiring concise.
	SessionSecret     string
	SessionTTLMinutes int
	CookieSecure      bool
	OperatorAllowlist []string
	// TrustedOperator enables no-login trusted access: GET /api/v1/session mints
	// a session for this operator instead of returning 401. Empty keeps OAuth as
	// the only entry.
	TrustedOperator string

	HTTPClient *http.Client
	Clock      auth.Clock
}

// AuthConfig is a descriptive alias for callers that do not want a provider
// name in their route assembly code.
type AuthConfig = FeishuOAuthConfig

// OAuthStore is the one-time state half of the persistence boundary.
type OAuthStore interface {
	CreateWebOAuthState(context.Context, store.WebOAuthState) (store.WebOAuthState, error)
	ConsumeWebOAuthState(context.Context, string, time.Time) (store.WebOAuthState, error)
}

// FeishuAuthStore combines OAuth state persistence with browser session DAO
// methods. *store.DB satisfies this interface directly.
type FeishuAuthStore interface {
	auth.SessionStore
	OAuthStore
}

// AuthOption allows tests to inject transports/clocks without modifying global
// state. Config fields remain available for production wiring.
type AuthOption func(*FeishuAuthAPI)

func WithAuthHTTPClient(client *http.Client) AuthOption {
	return func(h *FeishuAuthAPI) {
		if client != nil {
			h.httpClient = client
		}
	}
}

func WithAuthClock(clock auth.Clock) AuthOption {
	return func(h *FeishuAuthAPI) {
		if clock != nil {
			h.clock = clock
		}
	}
}

func WithOAuthEndpoints(authorizeURL, tokenURL, userInfoURL string) AuthOption {
	return func(h *FeishuAuthAPI) {
		if strings.TrimSpace(authorizeURL) != "" {
			h.cfg.AuthorizeURL = strings.TrimSpace(authorizeURL)
		}
		if strings.TrimSpace(tokenURL) != "" {
			h.cfg.TokenURL = strings.TrimSpace(tokenURL)
		}
		if strings.TrimSpace(userInfoURL) != "" {
			h.cfg.UserInfoURL = strings.TrimSpace(userInfoURL)
		}
	}
}

// FeishuAuthAPI serves OAuth start/callback, session inspection and logout.
// Handle is the GoFrame adapter; ServeHTTP is the testable standard-library
// implementation used by every route.
type FeishuAuthAPI struct {
	store      FeishuAuthStore
	cfg        FeishuOAuthConfig
	sessions   *auth.SessionManager
	httpClient *http.Client
	clock      auth.Clock
	initErr    error
}

// AuthFeishuAPI is a readable alias for route registries.
type AuthFeishuAPI = FeishuAuthAPI

func NewFeishuAuthAPI(s FeishuAuthStore, cfg FeishuOAuthConfig, options ...AuthOption) *FeishuAuthAPI {
	h := &FeishuAuthAPI{store: s, cfg: cfg}
	if cfg.HTTPClient != nil {
		h.httpClient = cfg.HTTPClient
	} else {
		h.httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	h.clock = cfg.Clock
	if h.clock == nil {
		h.clock = auth.ClockFunc(time.Now)
	}

	sessionCfg := cfg.Session
	if len(sessionCfg.Secret) == 0 && sessionCfg.SessionSecret == "" {
		sessionCfg.SessionSecret = cfg.SessionSecret
	}
	if sessionCfg.TTL <= 0 && sessionCfg.SessionTTL <= 0 && sessionCfg.TTLMinutes <= 0 {
		sessionCfg.TTLMinutes = cfg.SessionTTLMinutes
	}
	if !sessionCfg.CookieSecure {
		sessionCfg.CookieSecure = cfg.CookieSecure
	}
	if len(sessionCfg.OperatorAllowlist) == 0 {
		sessionCfg.OperatorAllowlist = append([]string(nil), cfg.OperatorAllowlist...)
	}
	if strings.TrimSpace(sessionCfg.TrustedOperator) == "" {
		sessionCfg.TrustedOperator = cfg.TrustedOperator
	}
	if sessionCfg.Clock == nil {
		sessionCfg.Clock = h.clock
	}
	h.sessions, h.initErr = auth.NewSessionManagerWithError(s, sessionCfg)
	for _, option := range options {
		if option != nil {
			option(h)
		}
	}
	if h.sessions != nil && h.sessions.Ready() == nil {
		// Keep one clock source for OAuth expiry and sessions. Options are applied
		// after construction, so an injected clock must also update the manager.
		if h.clock != nil {
			sessionCfg.Clock = h.clock
			h.sessions = auth.NewSessionManager(s, sessionCfg)
		}
	}
	if h.sessions == nil && h.initErr == nil {
		h.initErr = errors.New("auth: session manager unavailable")
	}
	return h
}

// NewAuthFeishuAPI is an alias used by route assembly code.
func NewAuthFeishuAPI(s FeishuAuthStore, cfg FeishuOAuthConfig, options ...AuthOption) *FeishuAuthAPI {
	return NewFeishuAuthAPI(s, cfg, options...)
}

// NewFeishuAuthAPIWithError is the strict constructor useful for startup checks.
// Trusted access performs no OAuth round trip, so OAuth credentials are not a
// startup requirement in that mode; /auth/feishu/* still fails closed through
// ready() if it is ever called without credentials.
func NewFeishuAuthAPIWithError(s FeishuAuthStore, cfg FeishuOAuthConfig, options ...AuthOption) (*FeishuAuthAPI, error) {
	h := NewFeishuAuthAPI(s, cfg, options...)
	check := h.ready
	if strings.TrimSpace(cfg.TrustedOperator) != "" {
		check = h.sessionReady
	}
	if err := check(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *FeishuAuthAPI) Handle(r *ghttp.Request) {
	if r == nil || r.Response == nil || r.Request == nil {
		return
	}
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *FeishuAuthAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r == nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	switch r.URL.Path {
	case "/auth/feishu/start":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.start(w, r)
	case "/auth/feishu/callback":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.callback(w, r)
	case "/api/v1/session":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.session(w, r)
	case "/auth/logout":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.logout(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *FeishuAuthAPI) ready() error {
	if h == nil {
		return errors.New("auth: handler is nil")
	}
	if h.initErr != nil {
		return h.initErr
	}
	if h.sessions == nil {
		return errors.New("auth: session manager unavailable")
	}
	if err := h.sessions.Ready(); err != nil {
		return err
	}
	if strings.TrimSpace(h.clientID()) == "" || strings.TrimSpace(h.clientSecret()) == "" {
		return errors.New("auth: Feishu OAuth client credentials are required")
	}
	if h.httpClient == nil {
		return errors.New("auth: OAuth HTTP client is required")
	}
	return nil
}

func (h *FeishuAuthAPI) clientID() string {
	if strings.TrimSpace(h.cfg.ClientID) != "" {
		return strings.TrimSpace(h.cfg.ClientID)
	}
	return strings.TrimSpace(h.cfg.AppID)
}

func (h *FeishuAuthAPI) clientSecret() string {
	if strings.TrimSpace(h.cfg.ClientSecret) != "" {
		return strings.TrimSpace(h.cfg.ClientSecret)
	}
	return strings.TrimSpace(h.cfg.AppSecret)
}

func (h *FeishuAuthAPI) now() time.Time {
	if h != nil && h.clock != nil {
		return h.clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (h *FeishuAuthAPI) redirectURI(_ *http.Request) string {
	if value := strings.TrimSpace(h.cfg.RedirectURI); value != "" {
		return value
	}
	if base := strings.TrimRight(strings.TrimSpace(h.cfg.BaseURL), "/"); base != "" {
		return base + "/auth/feishu/callback"
	}
	return ""
}

func randomOAuthValue(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("auth: generate OAuth value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

type oauthStatePayload struct {
	Verifier    string `json:"verifier"`
	NonceHash   string `json:"nonce_hash"`
	SuccessPath string `json:"success_path"`
}

func (h *FeishuAuthAPI) start(w http.ResponseWriter, r *http.Request) {
	if err := h.ready(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "OAuth is unavailable")
		return
	}
	redirectURI := h.redirectURI(r)
	if redirectURI == "" {
		writeError(w, http.StatusServiceUnavailable, "OAuth redirect is unavailable")
		return
	}
	state, err := randomOAuthValue(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "OAuth state generation failed")
		return
	}
	nonce, err := randomOAuthValue(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "OAuth nonce generation failed")
		return
	}
	verifier, err := randomOAuthValue(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "OAuth verifier generation failed")
		return
	}
	payloadBytes, err := json.Marshal(oauthStatePayload{Verifier: verifier, NonceHash: auth.HashSecret(nonce), SuccessPath: safeSuccessPath(r.URL.Query().Get("next"))})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "OAuth state protection failed")
		return
	}
	ciphertext, err := h.sessions.EncryptVerifier(string(payloadBytes))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "OAuth state protection failed")
		return
	}
	challengeBytes := sha256Bytes([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes)
	now := h.now()
	ttl := h.cfg.StateTTL
	if ttl <= 0 {
		ttl = defaultOAuthStateTTL
	}
	_, err = h.store.CreateWebOAuthState(r.Context(), store.WebOAuthState{StateHash: auth.HashSecret(state), CodeVerifierCiphertext: ciphertext, RedirectURI: redirectURI, ExpiresAt: now.Add(ttl), CreatedAt: now})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "OAuth state persistence failed")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthNonceCookieName, Value: nonce, Path: "/", Expires: now.Add(ttl), MaxAge: int(ttl / time.Second), HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	authorizeURL := h.cfg.AuthorizeURL
	if strings.TrimSpace(authorizeURL) == "" {
		authorizeURL = defaultFeishuAuthorizeURL
	}
	query := url.Values{}
	query.Set("client_id", h.clientID())
	query.Set("response_type", "code")
	query.Set("redirect_uri", redirectURI)
	scope := strings.TrimSpace(h.cfg.Scope)
	if scope == "" {
		scope = "offline_access"
	}
	query.Set("scope", scope)
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	location, err := url.Parse(authorizeURL)
	if err != nil || location.Scheme == "" || location.Host == "" {
		writeError(w, http.StatusServiceUnavailable, "OAuth authorization endpoint is invalid")
		return
	}
	location.RawQuery = query.Encode()
	http.Redirect(w, r, location.String(), http.StatusFound)
}

func sha256Bytes(value []byte) []byte {
	sum := sha256.Sum256(value)
	return sum[:]
}

func (h *FeishuAuthAPI) callback(w http.ResponseWriter, r *http.Request) {
	if err := h.ready(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "OAuth is unavailable")
		return
	}
	query := r.URL.Query()
	code := strings.TrimSpace(query.Get("code"))
	stateValue := strings.TrimSpace(query.Get("state"))
	if code == "" || stateValue == "" {
		writeError(w, http.StatusBadRequest, "OAuth code and state are required")
		return
	}
	nonceCookie, cookieErr := r.Cookie(oauthNonceCookieName)
	if cookieErr != nil || strings.TrimSpace(nonceCookie.Value) == "" {
		writeError(w, http.StatusBadRequest, "OAuth browser state is missing")
		return
	}
	state, err := h.store.ConsumeWebOAuthState(r.Context(), auth.HashSecret(stateValue), h.now())
	if err != nil {
		if errors.Is(err, store.ErrWebOAuthStateNotFound) || errors.Is(err, store.ErrWebOAuthStateExpired) {
			writeError(w, http.StatusBadRequest, "OAuth state is invalid or expired")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "OAuth state lookup failed")
		return
	}
	plaintext, err := h.sessions.DecryptVerifier(state.CodeVerifierCiphertext)
	if err != nil {
		writeError(w, http.StatusBadRequest, "OAuth state is invalid")
		return
	}
	var payload oauthStatePayload
	if json.Unmarshal([]byte(plaintext), &payload) != nil || strings.TrimSpace(payload.Verifier) == "" || subtle.ConstantTimeCompare([]byte(auth.HashSecret(strings.TrimSpace(nonceCookie.Value))), []byte(payload.NonceHash)) != 1 {
		writeError(w, http.StatusBadRequest, "OAuth browser state is invalid")
		return
	}
	token, err := h.exchangeCode(r.Context(), code, state.RedirectURI, payload.Verifier)
	if err != nil {
		writeError(w, http.StatusBadGateway, "OAuth token exchange failed")
		return
	}
	actor, err := h.fetchActor(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusBadGateway, "OAuth identity lookup failed")
		return
	}
	credentials, err := h.sessions.CreateSession(r.Context(), actor)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "session creation failed")
		return
	}
	h.sessions.SetCookies(w, credentials)
	http.SetCookie(w, &http.Cookie{Name: oauthNonceCookieName, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0).UTC(), HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, safeSuccessPath(payload.SuccessPath), http.StatusFound)
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	Code        int    `json:"code"`
	Msg         string `json:"msg"`
	Data        struct {
		AccessToken string `json:"access_token"`
	} `json:"data"`
}

func (h *FeishuAuthAPI) exchangeCode(ctx context.Context, code, redirectURI, verifier string) (string, error) {
	endpoint := h.cfg.TokenURL
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultFeishuTokenURL
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", h.clientID())
	form.Set("client_secret", h.clientSecret())
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("OAuth token request could not be created")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", errors.New("OAuth token request failed")
	}
	defer resp.Body.Close()
	var payload tokenResponse
	if err := decodeOAuthJSON(resp, &payload); err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("OAuth token response was invalid")
	}
	accessToken := strings.TrimSpace(payload.AccessToken)
	if accessToken == "" {
		accessToken = strings.TrimSpace(payload.Data.AccessToken)
	}
	if accessToken == "" || (payload.Code != 0 && payload.Code != 200) {
		return "", errors.New("OAuth token response was invalid")
	}
	return accessToken, nil
}

type actorResponse struct {
	OpenID    string `json:"open_id"`
	Name      string `json:"name"`
	TenantKey string `json:"tenant_key"`
	Code      int    `json:"code"`
	Data      struct {
		OpenID    string `json:"open_id"`
		Name      string `json:"name"`
		TenantKey string `json:"tenant_key"`
	} `json:"data"`
}

func (h *FeishuAuthAPI) fetchActor(ctx context.Context, accessToken string) (auth.Actor, error) {
	endpoint := h.cfg.UserInfoURL
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultFeishuUserInfoURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return auth.Actor{}, errors.New("OAuth user-info request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return auth.Actor{}, errors.New("OAuth user-info request failed")
	}
	defer resp.Body.Close()
	var payload actorResponse
	if err := decodeOAuthJSON(resp, &payload); err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return auth.Actor{}, errors.New("OAuth user-info response was invalid")
	}
	openID := strings.TrimSpace(payload.Data.OpenID)
	name := strings.TrimSpace(payload.Data.Name)
	tenant := strings.TrimSpace(payload.Data.TenantKey)
	if openID == "" {
		openID = strings.TrimSpace(payload.OpenID)
		name = strings.TrimSpace(payload.Name)
		tenant = strings.TrimSpace(payload.TenantKey)
	}
	if openID == "" || payload.Code != 0 && payload.Code != 200 {
		return auth.Actor{}, errors.New("OAuth user-info response was invalid")
	}
	if name == "" {
		name = openID
	}
	return auth.Actor{ID: openID, Name: name, TenantKey: tenant}, nil
}

func decodeOAuthJSON(resp *http.Response, target any) error {
	if resp == nil || resp.Body == nil {
		return errors.New("empty OAuth response")
	}
	const maxOAuthBody = 128 * 1024
	return json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBody)).Decode(target)
}

func (h *FeishuAuthAPI) session(w http.ResponseWriter, r *http.Request) {
	if err := h.sessionReady(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "session is unavailable")
		return
	}
	actor, session, err := h.sessions.Authenticate(r.Context(), r)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			// Trusted mode has no login step: mint the session here instead of
			// asking the browser to complete an OAuth round trip.
			if trusted := auth.TrustedActor(h.cfg.TrustedOperator); trusted.ID != "" {
				h.issueTrustedSession(w, r, trusted)
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "session lookup failed")
		return
	}
	csrf := ""
	if cookie, cookieErr := r.Cookie(auth.CSRFCookieName); cookieErr == nil {
		candidate := strings.TrimSpace(cookie.Value)
		if candidate != "" && subtleEqual(auth.HashSecret(candidate), session.CSRFTokenHash) {
			csrf = candidate
		}
	}
	writeSessionJSON(w, actor, csrf, h.sessions.IsOperator(actor.ID))
}

// issueTrustedSession creates and returns a session for the configured trusted
// operator. The raw CSRF token is returned in the body because the request that
// triggered minting cannot carry the cookie that is being set on this response.
func (h *FeishuAuthAPI) issueTrustedSession(w http.ResponseWriter, r *http.Request, actor auth.Actor) {
	credentials, err := h.sessions.CreateSession(r.Context(), actor)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "session issue failed")
		return
	}
	h.sessions.SetCookies(w, credentials)
	writeSessionJSON(w, actor, credentials.CSRFToken, h.sessions.IsOperator(actor.ID))
}

func writeSessionJSON(w http.ResponseWriter, actor auth.Actor, csrf string, allowed bool) {
	writeJSON(w, http.StatusOK, map[string]any{
		"actor":            actor,
		"csrf_token":       csrf,
		"can_operate":      allowed,
		"operator_allowed": allowed,
	})
}

func subtleEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var value byte
	for i := range left {
		value |= left[i] ^ right[i]
	}
	return value == 0
}

func safeSuccessPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return "/"
	}
	return value
}
func (h *FeishuAuthAPI) sessionReady() error {
	if h == nil {
		return errors.New("auth: handler is nil")
	}
	if h.initErr != nil {
		return h.initErr
	}
	if h.sessions == nil {
		return errors.New("auth: session manager unavailable")
	}
	return h.sessions.Ready()
}

func (h *FeishuAuthAPI) logout(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.sessions == nil {
		writeJSON(w, http.StatusNoContent, nil)
		return
	}
	_, session, err := h.sessions.Authenticate(r.Context(), r)
	if errors.Is(err, auth.ErrUnauthenticated) {
		h.sessions.ClearCookies(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "logout session lookup failed")
		return
	}
	if err := h.sessions.CheckCSRF(r, session); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}
	if err := h.sessions.Revoke(r.Context(), r); err != nil {
		writeError(w, http.StatusServiceUnavailable, "logout failed")
		return
	}
	h.sessions.ClearCookies(w)
	w.WriteHeader(http.StatusNoContent)
}
