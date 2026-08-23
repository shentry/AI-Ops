// Package auth implements the server-side browser session boundary used by the
// Web control room. Browser cookies contain opaque random values; only hashes are
// persisted by the store.
package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"oncall-agent/internal/store"
)

const (
	SessionCookieName = "oncall_session"
	CSRFCookieName    = "oncall_csrf"
	CSRFHeaderName    = "X-CSRF-Token"

	defaultSessionTTL = 8 * time.Hour
)

var (
	ErrSessionUnavailable = errors.New("auth: session manager unavailable")
	ErrUnauthenticated    = errors.New("auth: unauthenticated")
	ErrCSRF               = errors.New("auth: csrf validation failed")
	ErrOperatorForbidden  = errors.New("auth: operator is not allowed")
)

// Clock is injectable so OAuth/session tests do not depend on wall-clock time.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function to Clock.
type ClockFunc func() time.Time

func (f ClockFunc) Now() time.Time { return f() }

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// SessionConfig contains only Web-session settings. Keeping this type local to
// auth avoids coupling the security boundary to the process-wide YAML package.
type SessionConfig struct {
	// Secret and SessionSecret are equivalent. Secret is preferred; the string
	// form makes wiring directly from environment-backed config convenient.
	Secret        []byte
	SessionSecret string

	TTL        time.Duration
	SessionTTL time.Duration
	TTLMinutes int

	CookieSecure      bool
	OperatorAllowlist []string
	// TrustedOperator names the no-login identity. It is implicitly an allowed
	// operator; leaving it empty keeps the allowlist as the only source.
	TrustedOperator string
	Clock           Clock
}

// Actor is the identity obtained from Feishu user_info and stored in a session.
type Actor struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	TenantKey string `json:"tenant_key,omitempty"`
}

// TrustedActorPrefix marks identities minted by the no-login trusted mode so an
// approval audit row is never mistaken for a real Feishu open_id.
const TrustedActorPrefix = "local:"

// TrustedActor builds the identity used by trusted (no-login) Web access.
// An empty name yields a zero Actor, which CreateSession rejects.
func TrustedActor(name string) Actor {
	name = strings.TrimSpace(name)
	if name == "" {
		return Actor{}
	}
	return Actor{ID: TrustedActorPrefix + name, Name: name}
}

// SessionCredentials are returned only to the caller that is about to set
// response cookies. Token and CSRF values are never written to the database.
type SessionCredentials struct {
	Token     string
	CSRFToken string
	ExpiresAt time.Time
	Session   store.WebSession
}

// SessionManager authenticates browser requests and performs the small amount
// of cryptography needed to protect a one-time PKCE verifier.
type SessionManager struct {
	store    SessionStore
	key      []byte
	ttl      time.Duration
	secure   bool
	allowset map[string]struct{}
	clock    Clock
	initErr  error
}

// Manager is retained as a descriptive alias for callers that prefer the short
// name; both names denote the exact same implementation.
type Manager = SessionManager

// SessionStore is the minimum DAO required by browser authentication. OAuth
// state persistence is deliberately a separate interface so a session-only
// middleware can be used by tests and by read-only endpoints.
type SessionStore interface {
	CreateWebSession(context.Context, store.WebSession) (store.WebSession, error)
	GetWebSessionByTokenHash(context.Context, string, time.Time) (store.WebSession, error)
	RevokeWebSession(context.Context, string, time.Time) error
}

type sessionTouchStore interface {
	TouchWebSession(context.Context, string, time.Time) error
}

// NewSessionManager constructs a manager. Configuration errors are retained and
// returned by Ready/Issue/Authenticate rather than panicking during process
// assembly; config.Load is responsible for fail-fast validation in production.
func NewSessionManager(s SessionStore, cfg SessionConfig) *SessionManager {
	m, err := NewSessionManagerWithError(s, cfg)
	if err != nil {
		return &SessionManager{store: s, clock: wallClock{}, initErr: err}
	}
	return m
}

// NewSessionManagerWithError is the strict constructor for startup and tests.
func NewSessionManagerWithError(s SessionStore, cfg SessionConfig) (*SessionManager, error) {
	if s == nil {
		return nil, errors.New("auth: session store is required")
	}
	secret := cfg.Secret
	if len(secret) == 0 && cfg.SessionSecret != "" {
		secret = []byte(cfg.SessionSecret)
	}
	if len(secret) < 32 {
		return nil, errors.New("auth: session secret must be at least 32 bytes")
	}
	key := make([]byte, 32)
	// Hashing instead of truncating permits a long configured secret while
	// retaining the required 256-bit AES-GCM key size.
	sum := sha256.Sum256(secret)
	copy(key, sum[:])
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = cfg.SessionTTL
	}
	if ttl <= 0 && cfg.TTLMinutes > 0 {
		ttl = time.Duration(cfg.TTLMinutes) * time.Minute
	}
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	if ttl <= 0 {
		return nil, errors.New("auth: session ttl must be positive")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = wallClock{}
	}
	allowset := make(map[string]struct{}, len(cfg.OperatorAllowlist)+1)
	for _, id := range cfg.OperatorAllowlist {
		if id = strings.TrimSpace(id); id != "" {
			allowset[id] = struct{}{}
		}
	}
	// An empty allowlist is fail-closed, so the trusted identity is allowed here
	// rather than by every assembly site: a console that can enter but can never
	// approve is a silent misconfiguration.
	if trusted := TrustedActor(cfg.TrustedOperator); trusted.ID != "" {
		allowset[trusted.ID] = struct{}{}
	}
	return &SessionManager{
		store:    s,
		key:      key,
		ttl:      ttl,
		secure:   cfg.CookieSecure,
		allowset: allowset,
		clock:    clock,
	}, nil
}

// NewManager is an alias with the same strict construction behavior deferred to
// Ready, useful when wiring code wants a neutral manager name.
func NewManager(s SessionStore, cfg SessionConfig) *SessionManager {
	return NewSessionManager(s, cfg)
}

// Ready reports configuration errors retained by NewSessionManager.
func (m *SessionManager) Ready() error {
	if m == nil || m.initErr != nil {
		if m == nil {
			return ErrSessionUnavailable
		}
		return m.initErr
	}
	return nil
}

func (m *SessionManager) now() time.Time {
	if m == nil || m.clock == nil {
		return time.Now().UTC()
	}
	return m.clock.Now().UTC()
}

// HashSecret returns the stable SHA-256 hex representation used by the DAO.
// It is safe to use for session tokens, CSRF tokens and OAuth state separately.
func HashSecret(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func randomURLSecret(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("auth: random secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomID(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("auth: random id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// CreateSession creates a DB-backed session and returns its one-time raw cookie
// values. Actor fields are copied into the DB; no Feishu token is accepted here.
func (m *SessionManager) CreateSession(ctx context.Context, actor Actor) (SessionCredentials, error) {
	if err := m.Ready(); err != nil {
		return SessionCredentials{}, err
	}
	actor.ID = strings.TrimSpace(actor.ID)
	actor.Name = strings.TrimSpace(actor.Name)
	actor.TenantKey = strings.TrimSpace(actor.TenantKey)
	if actor.ID == "" {
		return SessionCredentials{}, errors.New("auth: actor id is required")
	}
	if actor.Name == "" {
		actor.Name = actor.ID
	}
	token, err := randomURLSecret(32)
	if err != nil {
		return SessionCredentials{}, err
	}
	csrf, err := randomURLSecret(32)
	if err != nil {
		return SessionCredentials{}, err
	}
	id, err := randomID(16)
	if err != nil {
		return SessionCredentials{}, err
	}
	now := m.now()
	expires := now.Add(m.ttl)
	session := store.WebSession{
		ID:            id,
		TokenHash:     HashSecret(token),
		ActorID:       actor.ID,
		ActorName:     actor.Name,
		CSRFTokenHash: HashSecret(csrf),
		ExpiresAt:     expires,
		CreatedAt:     now,
		LastSeenAt:    now,
	}
	if actor.TenantKey != "" {
		tenant := actor.TenantKey
		session.TenantKey = &tenant
	}
	stored, err := m.store.CreateWebSession(ctx, session)
	if err != nil {
		return SessionCredentials{}, err
	}
	return SessionCredentials{Token: token, CSRFToken: csrf, ExpiresAt: expires, Session: stored}, nil
}

// IssueSession is a descriptive alias used by HTTP handlers.
func (m *SessionManager) IssueSession(ctx context.Context, actor Actor) (SessionCredentials, error) {
	return m.CreateSession(ctx, actor)
}

// ActorFromSession converts the persistence model without exposing any secret.
func ActorFromSession(session store.WebSession) Actor {
	actor := Actor{ID: session.ActorID, Name: session.ActorName}
	if session.TenantKey != nil {
		actor.TenantKey = *session.TenantKey
	}
	return actor
}

// Authenticate validates the HttpOnly session cookie and returns its actor.
func (m *SessionManager) Authenticate(ctx context.Context, r *http.Request) (Actor, store.WebSession, error) {
	if err := m.Ready(); err != nil {
		return Actor{}, store.WebSession{}, err
	}
	if r == nil {
		return Actor{}, store.WebSession{}, ErrUnauthenticated
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return Actor{}, store.WebSession{}, ErrUnauthenticated
	}
	token := strings.TrimSpace(cookie.Value)
	session, err := m.store.GetWebSessionByTokenHash(ctx, HashSecret(token), m.now())
	if err != nil {
		if errors.Is(err, store.ErrWebSessionNotFound) {
			return Actor{}, store.WebSession{}, ErrUnauthenticated
		}
		return Actor{}, store.WebSession{}, err
	}
	if touch, ok := m.store.(sessionTouchStore); ok {
		// Last-seen is an audit hint, not an authentication dependency. A transient
		// failure must not turn a valid session into a spurious logout.
		_ = touch.TouchWebSession(ctx, HashSecret(token), m.now())
	}
	return ActorFromSession(session), session, nil
}

// RequireSession is middleware that injects the authenticated actor/session into
// the request context. It intentionally does not enforce operator allowlisting;
// read-only Web APIs use the same identity boundary.
func (m *SessionManager) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, session, err := m.Authenticate(r.Context(), r)
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrSessionUnavailable) {
				writeAuthError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeAuthError(w, http.StatusServiceUnavailable, "session unavailable")
			return
		}
		ctx := context.WithValue(r.Context(), actorContextKey{}, actor)
		ctx = context.WithValue(ctx, sessionContextKey{}, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Middleware is an alias for RequireSession.
func (m *SessionManager) Middleware(next http.Handler) http.Handler { return m.RequireSession(next) }

// SessionMiddleware is a package-level convenience for route assembly.
func SessionMiddleware(m *SessionManager, next http.Handler) http.Handler {
	if m == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeAuthError(w, http.StatusUnauthorized, "unauthorized")
		})
	}
	return m.RequireSession(next)
}

type actorContextKey struct{}
type sessionContextKey struct{}

func ActorFromContext(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(Actor)
	return actor, ok
}

func SessionFromContext(ctx context.Context) (store.WebSession, bool) {
	session, ok := ctx.Value(sessionContextKey{}).(store.WebSession)
	return session, ok
}

// ActorFromRequest returns an actor previously installed by RequireSession.
func ActorFromRequest(r *http.Request) (Actor, bool) {
	if r == nil {
		return Actor{}, false
	}
	return ActorFromContext(r.Context())
}

// IsOperator checks the exact Feishu open_id allowlist. An empty allowlist is
// fail-closed: authenticated users can view, but none can approve.
func (m *SessionManager) IsOperator(actorID string) bool {
	if m == nil {
		return false
	}
	_, ok := m.allowset[strings.TrimSpace(actorID)]
	return ok
}

// CheckOperator validates an actor against the configured allowlist.
func (m *SessionManager) CheckOperator(actor Actor) error {
	if !m.IsOperator(actor.ID) {
		return ErrOperatorForbidden
	}
	return nil
}

// CheckCSRF enforces the double-submit cookie and the hash persisted for the
// authenticated session. Constant-time comparisons avoid leaking token bytes.
func (m *SessionManager) CheckCSRF(r *http.Request, session store.WebSession) error {
	if r == nil {
		return ErrCSRF
	}
	header := strings.TrimSpace(r.Header.Get(CSRFHeaderName))
	cookie, err := r.Cookie(CSRFCookieName)
	if err != nil {
		return ErrCSRF
	}
	cookieValue := strings.TrimSpace(cookie.Value)
	if header == "" || cookieValue == "" || subtle.ConstantTimeCompare([]byte(header), []byte(cookieValue)) != 1 {
		return ErrCSRF
	}
	if subtle.ConstantTimeCompare([]byte(HashSecret(header)), []byte(session.CSRFTokenHash)) != 1 {
		return ErrCSRF
	}
	return nil
}

// SetCookies writes the two browser cookies. Session is HttpOnly; CSRF is
// intentionally readable by the frontend so it can send the required header.
func (m *SessionManager) SetCookies(w http.ResponseWriter, credentials SessionCredentials) {
	if w == nil {
		return
	}
	maxAge := int(m.ttl / time.Second)
	if maxAge < 1 {
		maxAge = 1
	}
	expires := credentials.ExpiresAt
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    credentials.Token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    credentials.CSRFToken,
		Path:     "/",
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: false,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookies revokes browser-side cookie state. The DB revocation is handled
// separately by Revoke, allowing logout to remain idempotent.
func (m *SessionManager) ClearCookies(w http.ResponseWriter) {
	if w == nil {
		return
	}
	for _, name := range []string{SessionCookieName, CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			Expires:  time.Unix(1, 0).UTC(),
			MaxAge:   -1,
			HttpOnly: name == SessionCookieName,
			Secure:   m != nil && m.secure,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// Revoke logs out the session represented by the request cookie. Missing or
// already-revoked sessions are treated as successful logout.
func (m *SessionManager) Revoke(ctx context.Context, r *http.Request) error {
	if m == nil || r == nil {
		return nil
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return nil
	}
	return m.store.RevokeWebSession(ctx, HashSecret(strings.TrimSpace(cookie.Value)), m.now())
}

// EncryptVerifier protects a PKCE verifier at rest using AES-GCM. The returned
// value is URL-safe base64 of nonce || ciphertext; plaintext never reaches the
// store boundary.
func (m *SessionManager) EncryptVerifier(plaintext string) (string, error) {
	if err := m.Ready(); err != nil {
		return "", err
	}
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return "", fmt.Errorf("auth: verifier cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("auth: verifier cipher: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("auth: verifier nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// DecryptVerifier reverses EncryptVerifier and rejects malformed or tampered
// state. It is only called after the one-time state has been consumed.
func (m *SessionManager) DecryptVerifier(encoded string) (string, error) {
	if err := m.Ready(); err != nil {
		return "", err
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("auth: invalid verifier ciphertext")
	}
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return "", fmt.Errorf("auth: verifier cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("auth: verifier cipher: %w", err)
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("auth: invalid verifier ciphertext")
	}
	nonce, ciphertext := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("auth: invalid verifier ciphertext")
	}
	return string(plaintext), nil
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+message+`"}`+"\n")
}
