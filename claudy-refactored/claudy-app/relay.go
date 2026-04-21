package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// relayHTTP has a bounded timeout so auth calls can't hang the daemon loop.
var relayHTTP = &http.Client{Timeout: 15 * time.Second}

type authResponse struct {
	Token string `json:"token"`
	Error string `json:"error"`
}

func normalizeRelayURL(s string) string {
	return strings.TrimRight(s, "/")
}

// relayLogin authenticates against the relay and returns a JWT.
func relayLogin(relayURL, username, password string) (string, error) {
	return relayPostAuth(relayURL+"/auth/login", username, password, http.StatusOK)
}

// relayRegister creates a new account.
func relayRegister(relayURL, username, password string) error {
	_, err := relayPostAuth(relayURL+"/auth/register", username, password, http.StatusCreated)
	return err
}

func relayPostAuth(endpoint, username, password string, wantStatus int) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := relayHTTP.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("cannot reach server: %w", err)
	}
	defer resp.Body.Close()

	var r authResponse
	_ = json.NewDecoder(resp.Body).Decode(&r)

	if resp.StatusCode != wantStatus {
		if r.Error != "" {
			return "", fmt.Errorf("%s", r.Error)
		}
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return r.Token, nil
}

// buildWSURL upgrades the relay base URL to a /tunnel WS(S) endpoint with the token.
func buildWSURL(relayURL, token string) (string, error) {
	u, err := url.Parse(relayURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = "/tunnel"
	q := url.Values{}
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
