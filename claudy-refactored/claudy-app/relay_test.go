package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeRelayURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http://relay.example", "http://relay.example"},
		{"http://relay.example/", "http://relay.example"},
		{"http://relay.example///", "http://relay.example"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeRelayURL(c.in); got != c.want {
			t.Errorf("normalizeRelayURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildWSURL(t *testing.T) {
	cases := []struct {
		relay, token, want string
	}{
		{"http://relay.example", "abc", "ws://relay.example/tunnel?token=abc"},
		{"https://relay.example", "abc", "wss://relay.example/tunnel?token=abc"},
		{"http://relay.example:8080", "t+1", "ws://relay.example:8080/tunnel?token=t%2B1"},
	}
	for _, c := range cases {
		got, err := buildWSURL(c.relay, c.token)
		if err != nil {
			t.Fatalf("buildWSURL(%q): %v", c.relay, err)
		}
		if got != c.want {
			t.Errorf("buildWSURL(%q, %q) = %q, want %q", c.relay, c.token, got, c.want)
		}
	}
}

func TestBuildWSURL_BadURL(t *testing.T) {
	if _, err := buildWSURL("://bad", "x"); err == nil {
		t.Error("expected error for malformed URL")
	}
}

func TestRelayLogin_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/login" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		if req["username"] != "alice" || req["password"] != "secret" {
			t.Errorf("unexpected body: %s", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "jwt-xyz"})
	}))
	defer srv.Close()

	tok, err := relayLogin(srv.URL, "alice", "secret")
	if err != nil {
		t.Fatalf("relayLogin: %v", err)
	}
	if tok != "jwt-xyz" {
		t.Errorf("token = %q", tok)
	}
}

func TestRelayLogin_ErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad credentials"})
	}))
	defer srv.Close()

	_, err := relayLogin(srv.URL, "alice", "wrong")
	if err == nil || !strings.Contains(err.Error(), "bad credentials") {
		t.Errorf("want error containing 'bad credentials', got %v", err)
	}
}

func TestRelayLogin_Unreachable(t *testing.T) {
	_, err := relayLogin("http://127.0.0.1:1", "a", "b")
	if err == nil {
		t.Fatal("expected error on unreachable host")
	}
	if !strings.Contains(err.Error(), "cannot reach server") {
		t.Errorf("unexpected error wrapping: %v", err)
	}
}

func TestRelayRegister_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/register" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	if err := relayRegister(srv.URL, "alice", "pw"); err != nil {
		t.Errorf("relayRegister: %v", err)
	}
}

func TestRelayRegister_WrongStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "username taken"})
	}))
	defer srv.Close()

	err := relayRegister(srv.URL, "alice", "pw")
	if err == nil || !strings.Contains(err.Error(), "username taken") {
		t.Errorf("want 'username taken', got %v", err)
	}
}
