// Package auth implements local username/password authentication with
// argon2id password hashing and DB-backed sessions (D16). The authn
// surface is isolated behind a small interface so authentik OIDC can be
// layered in later without touching handlers.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

var (
	// ErrNoUser is returned when the username is unknown. A generic
	// error is shown to the caller to avoid username enumeration.
	ErrNoUser = errors.New("no such user")
	// ErrBadPassword is returned on credential mismatch.
	ErrBadPassword = errors.New("invalid credentials")
)

// argon2id parameters (OWASP-recommended floor for low-user-count apps).
const (
	argon2Time    = 1
	argon2Memory  = 64 * 1024
	argon2Threads = 4
	argon2KeyLen  = 32
	argon2SaltLen = 16
)

// HashPassword hashes a plaintext password with argon2id.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	encSalt := base64.RawStdEncoding.EncodeToString(salt)
	encHash := base64.RawStdEncoding.EncodeToString(hash)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argon2Memory, argon2Time, argon2Threads, encSalt, encHash), nil
}

// CheckPassword verifies a plaintext password against a stored PHC hash.
func CheckPassword(stored, password string) error {
	parts := splitHash(stored)
	if len(parts) != 6 {
		return ErrBadPassword
	}
	var mem, time_, threads uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time_, &threads); err != nil {
		return ErrBadPassword
	}
	if threads == 0 || threads > 255 {
		return ErrBadPassword
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return ErrBadPassword
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return ErrBadPassword
	}
	got := argon2.IDKey([]byte(password), salt, time_, mem, uint8(threads), uint32(len(want)))
	equal := constantTimeEqual(got, want)
	if !equal {
		return ErrBadPassword
	}
	return nil
}

func splitHash(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '$' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	out = append(out, cur)
	return out
}

func constantTimeEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var x byte
	for i := range a {
		x |= a[i] ^ b[i]
	}
	return x == 0
}

// Session is a live, verified session.
type Session struct {
	ID        string
	Household int64
	UserID    int64
	CSRFToken string
	ExpiresAt time.Time
}

// Token generates a cryptographically random session id.
func Token() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const (
	// CookieName is the session cookie.
	CookieName = "eventally_session"
	// SessionTTL is how long a session lasts before expiry.
	SessionTTL = 30 * 24 * time.Hour
)

// SetSessionCookie writes the session cookie.
func SetSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   int(SessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure is set by the reverse proxy; for home-LAN http it stays
		// off, which is why the cookie is Strict same-site only.
	})
}

// ClearSessionCookie expires the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// Store persists and verifies sessions.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a session store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Create inserts a new session for the user.
func (s *Store) Create(ctx context.Context, household, userID int64, userAgent, ip string) (*Session, error) {
	id, err := Token()
	if err != nil {
		return nil, err
	}
	csrf, err := Token()
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(SessionTTL)
	_, err = s.pool.Exec(ctx,
		`INSERT INTO session (id, household_id, user_id, csrf_token, user_agent, ip, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, household, userID, csrf, userAgent, ip, expires)
	if err != nil {
		return nil, err
	}
	return &Session{ID: id, Household: household, UserID: userID, CSRFToken: csrf, ExpiresAt: expires}, nil
}

// Get loads a session by cookie id. Returns ErrNoUser-shaped nil when
// the session is unknown or expired (expired rows are deleted).
func (s *Store) Get(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, errors.New("no session")
	}
	var sess Session
	var expires time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, household_id, user_id, csrf_token, expires_at
		 FROM session WHERE id = $1`, id).
		Scan(&sess.ID, &sess.Household, &sess.UserID, &sess.CSRFToken, &expires)
	if err != nil {
		return nil, errors.New("unknown session")
	}
	if time.Now().After(expires) {
		s.Delete(ctx, id)
		return nil, errors.New("expired session")
	}
	sess.ExpiresAt = expires
	return &sess, nil
}

// Delete removes a session (logout or expiry).
func (s *Store) Delete(ctx context.Context, id string) {
	if id != "" {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM session WHERE id = $1`, id)
	}
}

// PruneExpired removes all expired sessions (called opportunistically).
func (s *Store) PruneExpired(ctx context.Context) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM session WHERE expires_at < now()`)
}

// SessionIDFromRequest reads the session cookie from a request.
func SessionIDFromRequest(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// RefererOrFallback builds a redirect URL: the Referer if it is same-site,
// otherwise the fallback.
func RefererOrFallback(r *http.Request, fallback string) string {
	ref := r.Referer()
	if ref == "" {
		return fallback
	}
	u, err := url.Parse(ref)
	if err != nil {
		return fallback
	}
	if u.Host != "" && u.Host != r.Host {
		return fallback
	}
	return u.String()
}
