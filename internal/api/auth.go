package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/config"
)

// Role is an operator's permission tier; each tier includes the ones below it.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

var roleRank = map[Role]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

// Actor is the server-determined identity of a request. Clients never name
// themselves: headers such as X-Operator are not identity.
type Actor struct {
	ID   string
	Name string
	Role Role
	// Machine marks the shared automation token (Alertmanager, scripts). It can
	// read and trigger diagnosis but never carries human authority.
	Machine bool
	// Source is how the identity was proven: web (session cookie) or api (Bearer).
	Source string
}

const (
	sessionCookie = "oncall_session"
	sessionTTL    = 12 * time.Hour
	// csrfHeader must accompany every cookie-authenticated request that changes
	// state. Browsers cannot attach it cross-site without a CORS preflight, which
	// this server never grants; SameSite=Strict is the second layer.
	csrfHeader = "X-Requested-With"
	csrfValue  = "oncall-console"
)

type operatorIdentity struct {
	id, name string
	role     Role
}

// Auth resolves every request to one identity: the machine token, an operator's
// personal Bearer token, or a signed session cookie issued for that token.
// Sessions are stateless and signed with a per-process key, so a restart signs
// everyone out and removing an operator from config revokes access immediately.
type Auth struct {
	machineToken string
	byTokenHash  map[string]operatorIdentity
	byID         map[string]operatorIdentity
	sessionKey   []byte
	secure       bool
	now          func() time.Time
}

// NewAuth builds the resolver from validated configuration. secureCookies must
// be true whenever the console is served over HTTPS.
func NewAuth(machineToken string, operators []config.OperatorConfig, secureCookies bool) (*Auth, error) {
	if strings.TrimSpace(machineToken) == "" {
		return nil, errors.New("api: machine token is required")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	a := &Auth{machineToken: machineToken, byTokenHash: map[string]operatorIdentity{}, byID: map[string]operatorIdentity{},
		sessionKey: key, secure: secureCookies, now: time.Now}
	for _, operator := range operators {
		identity := operatorIdentity{id: operator.ID, name: operator.Name, role: Role(operator.Role)}
		if identity.name == "" {
			identity.name = operator.ID
		}
		if roleRank[identity.role] == 0 {
			return nil, errors.New("api: invalid operator role")
		}
		a.byTokenHash[operator.TokenSHA256] = identity
		a.byID[operator.ID] = identity
	}
	return a, nil
}

// Authenticate returns the caller's identity. A cookie session without the CSRF
// header on a state-changing request is not authenticated.
func (a *Auth) Authenticate(r *http.Request) (Actor, bool) {
	if a == nil || r == nil {
		return Actor{}, false
	}
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.machineToken)) == 1 {
			return Actor{ID: "machine", Name: "automation", Role: RoleViewer, Machine: true, Source: "api"}, true
		}
		if identity, ok := a.operatorByToken(token); ok {
			return identity.actor("api"), true
		}
		return Actor{}, false
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return Actor{}, false
	}
	id, ok := a.verifySession(cookie.Value)
	if !ok {
		return Actor{}, false
	}
	identity, ok := a.byID[id]
	if !ok {
		return Actor{}, false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != csrfValue {
		return Actor{}, false
	}
	return identity.actor("web"), true
}

// Require authenticates and authorizes a request, writing 401/403 on failure.
// The machine identity satisfies only endpoints that explicitly allow it, and
// only at viewer level plus that endpoint's own automation purpose.
func (a *Auth) Require(w http.ResponseWriter, r *http.Request, role Role, allowMachine bool) (Actor, bool) {
	actor, ok := a.Authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return Actor{}, false
	}
	if actor.Machine {
		if !allowMachine {
			writeError(w, http.StatusForbidden, "automation token cannot perform this action")
			return Actor{}, false
		}
		return actor, true
	}
	if roleRank[actor.Role] < roleRank[role] {
		writeError(w, http.StatusForbidden, "role "+string(actor.Role)+" cannot perform this action")
		return Actor{}, false
	}
	return actor, true
}

// minTokenLength rejects low-entropy tokens even if their hash was configured.
// Generate personal tokens with: openssl rand -hex 32
const minTokenLength = 32

func (a *Auth) operatorByToken(token string) (operatorIdentity, bool) {
	if len(token) < minTokenLength {
		return operatorIdentity{}, false
	}
	sum := sha256.Sum256([]byte(token))
	identity, ok := a.byTokenHash[hex.EncodeToString(sum[:])]
	return identity, ok
}

func (i operatorIdentity) actor(source string) Actor {
	return Actor{ID: i.id, Name: i.name, Role: i.role, Source: source}
}

type sessionClaims struct {
	Subject   string `json:"sub"`
	ExpiresAt int64  `json:"exp"`
}

func (a *Auth) signSession(id string, expires time.Time) string {
	payload, _ := json.Marshal(sessionClaims{Subject: id, ExpiresAt: expires.Unix()})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Auth) verifySession(value string) (string, bool) {
	encoded, signature, ok := strings.Cut(value, ".")
	if !ok {
		return "", false
	}
	given, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(encoded))
	if !hmac.Equal(given, mac.Sum(nil)) {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	var claims sessionClaims
	if json.Unmarshal(payload, &claims) != nil || claims.Subject == "" || a.now().Unix() >= claims.ExpiresAt {
		return "", false
	}
	return claims.Subject, true
}

// SessionAPI exchanges a personal token for a session cookie (POST), reports the
// current identity (GET) and signs out (DELETE).
type SessionAPI struct {
	auth *Auth
}

func NewSessionAPI(auth *Auth) *SessionAPI { return &SessionAPI{auth: auth} }

func (h *SessionAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

type SessionDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role Role   `json:"role"`
}

func (h *SessionAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.auth == nil || r == nil {
		writeNotFound(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		actor, ok := h.auth.Require(w, r, RoleViewer, false)
		if ok {
			writeJSON(w, http.StatusOK, SessionDTO{ID: actor.ID, Name: actor.Name, Role: actor.Role})
		}
	case http.MethodPost:
		h.login(w, r)
	case http.MethodDelete:
		http.SetCookie(w, h.cookie("", time.Unix(0, 0)))
		w.WriteHeader(http.StatusNoContent)
	default:
		writeMethodNotAllowed(w)
	}
}

func (h *SessionAPI) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeader) != csrfValue {
		writeError(w, http.StatusForbidden, "missing request header")
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	if err := decodeBoundedJSON(r, &input, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	identity, ok := h.auth.operatorByToken(strings.TrimSpace(input.Token))
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	expires := h.auth.now().Add(sessionTTL)
	http.SetCookie(w, h.cookie(h.auth.signSession(identity.id, expires), expires))
	writeJSON(w, http.StatusOK, SessionDTO{ID: identity.id, Name: identity.name, Role: identity.role})
}

func (h *SessionAPI) cookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: value, Path: "/", Expires: expires, HttpOnly: true,
		Secure: h.auth.secure, SameSite: http.SameSiteStrictMode}
}
