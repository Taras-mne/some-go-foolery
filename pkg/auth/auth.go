// Package auth handles user registration, login, and JWT validation.
// Users are persisted as a JSON file on the relay server.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// User represents a registered account.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
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

// Register creates a new user. Returns an error if username is taken.
func (s *Store) Register(username, password string) error {
	if username == "" || password == "" {
		return errors.New("username and password are required")
	}
	if len(password) < 6 {
		return errors.New("password must be at least 6 characters")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[username]; exists {
		return errors.New("username already taken")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}

	s.users[username] = &User{
		ID:           hex.EncodeToString(b),
		Username:     username,
		PasswordHash: string(hash),
		CreatedAt:    time.Now(),
	}
	return s.save()
}

// Login verifies credentials and returns a signed JWT on success.
func (s *Store) Login(username, password string) (string, error) {
	s.mu.RLock()
	u, ok := s.users[username]
	s.mu.RUnlock()

	if !ok || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", errors.New("invalid credentials")
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": username,
		"uid": u.ID,
		"exp": time.Now().Add(30 * 24 * time.Hour).Unix(),
	})
	return token.SignedString(s.jwtSecret)
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
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": username,
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
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
}
