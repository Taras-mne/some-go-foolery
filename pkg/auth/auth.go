// Package auth handles user registration, login, and JWT validation.
// Users are persisted as a JSON file on the relay server.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// User represents a registered account.
type User struct {
	ID            string    `json:"id"`
	Username      string    `json:"username"`
	Email         string    `json:"email"`
	PasswordHash  string    `json:"password_hash"`
	EmailVerified bool      `json:"email_verified"`
	VerifyToken   string    `json:"verify_token,omitempty"`
	VerifyExpiry  time.Time `json:"verify_expiry,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Store is a thread-safe user store backed by a JSON file.
type Store struct {
	mu        sync.RWMutex
	path      string
	users     map[string]*User // username → user
	jwtSecret []byte
}

// NewStore loads (or creates) the user store at path.
func NewStore(path string, jwtSecret []byte) (*Store, error) {
	s := &Store{
		path:      path,
		users:     make(map[string]*User),
		jwtSecret: jwtSecret,
	}
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var list []*User
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	for _, u := range list {
		s.users[u.Username] = u
	}
	return nil
}

func (s *Store) save() error {
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

// Register creates a new user pending email verification.
// Returns the verification token that must be emailed to the user.
func (s *Store) Register(username, password, email string) (verifyToken string, err error) {
	if username == "" || password == "" || email == "" {
		return "", errors.New("username, password and email are required")
	}
	if len(password) < 6 {
		return "", errors.New("password must be at least 6 characters")
	}
	if !strings.Contains(email, "@") {
		return "", errors.New("invalid email address")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[username]; exists {
		return "", errors.New("username already taken")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}

	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", err
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	verifyToken = hex.EncodeToString(tokenBytes)

	s.users[username] = &User{
		ID:           hex.EncodeToString(idBytes),
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
		VerifyToken:  verifyToken,
		VerifyExpiry: time.Now().Add(24 * time.Hour),
		CreatedAt:    time.Now(),
	}
	return verifyToken, s.save()
}

// VerifyEmail marks the user as verified given a valid token.
// Returns the username on success.
func (s *Store) VerifyEmail(token string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, u := range s.users {
		if u.VerifyToken == token {
			if time.Now().After(u.VerifyExpiry) {
				return "", errors.New("verification token has expired")
			}
			u.EmailVerified = true
			u.VerifyToken = ""
			u.VerifyExpiry = time.Time{}
			return u.Username, s.save()
		}
	}
	return "", errors.New("invalid verification token")
}

// Login verifies credentials and returns a signed JWT on success.
// Returns an error if the user's email has not been verified.
func (s *Store) Login(username, password string) (string, error) {
	s.mu.RLock()
	u, ok := s.users[username]
	s.mu.RUnlock()

	if !ok || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", errors.New("invalid credentials")
	}

	// Legacy users (no email set) are treated as pre-verified.
	if u.Email != "" && !u.EmailVerified {
		return "", errors.New("email not verified — check your inbox")
	}

	return s.issueToken(u)
}

// IssueToken generates a signed JWT for an existing user without verifying the
// password. Used internally for loopback (localhost) bypass flows.
func (s *Store) IssueToken(username string) (string, error) {
	s.mu.RLock()
	u, ok := s.users[username]
	s.mu.RUnlock()
	if !ok {
		return "", errors.New("user not found")
	}
	return s.issueToken(u)
}

func (s *Store) issueToken(u *User) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": u.Username,
		"uid": u.ID,
		"exp": time.Now().Add(30 * 24 * time.Hour).Unix(),
	})
	return token.SignedString(s.jwtSecret)
}

// ValidateToken verifies a JWT and returns the username.
func (s *Store) ValidateToken(tokenStr string) (string, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return s.jwtSecret, nil
	})
	if err != nil || !token.Valid {
		return "", errors.New("invalid or expired token")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", errors.New("invalid token claims")
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", errors.New("missing subject in token")
	}
	return sub, nil
}

// ValidatePassword checks username+password directly (used for WebDAV Basic Auth).
func (s *Store) ValidatePassword(username, password string) bool {
	s.mu.RLock()
	u, ok := s.users[username]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	if u.Email != "" && !u.EmailVerified {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
}
