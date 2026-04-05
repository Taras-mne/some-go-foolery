// Package captcha verifies CAPTCHA tokens from Cloudflare Turnstile or hCaptcha.
package captcha

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Provider identifies the CAPTCHA vendor.
type Provider string

const (
	ProviderTurnstile Provider = "turnstile"
	ProviderHCaptcha  Provider = "hcaptcha"
)

// Config holds CAPTCHA settings read from environment variables.
type Config struct {
	Provider  Provider // CAPTCHA_PROVIDER — "turnstile" (default) or "hcaptcha"
	SiteKey   string   // CAPTCHA_SITE_KEY — sent to frontend for widget rendering
	SecretKey string   // CAPTCHA_SECRET   — used for server-side verification
}

// Enabled reports whether CAPTCHA is configured.
func (c Config) Enabled() bool {
	return c.SecretKey != ""
}

// Verify checks a CAPTCHA token returned by the browser widget.
// remoteIP is optional but improves accuracy; pass empty string to omit.
// Returns nil if CAPTCHA is disabled (SecretKey empty).
func Verify(cfg Config, token, remoteIP string) error {
	if !cfg.Enabled() {
		return nil
	}
	if token == "" {
		return fmt.Errorf("captcha token missing")
	}

	verifyURL := verifyEndpoint(cfg.Provider)

	resp, err := http.PostForm(verifyURL, url.Values{
		"secret":   {cfg.SecretKey},
		"response": {token},
		"remoteip": {remoteIP},
	})
	if err != nil {
		return fmt.Errorf("captcha verification request failed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Success bool     `json:"success"`
		Errors  []string `json:"error-codes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("captcha response decode failed: %w", err)
	}
	if !result.Success {
		if len(result.Errors) > 0 {
			return fmt.Errorf("captcha failed: %s", strings.Join(result.Errors, ", "))
		}
		return fmt.Errorf("captcha verification failed")
	}
	return nil
}

func verifyEndpoint(p Provider) string {
	switch p {
	case ProviderHCaptcha:
		return "https://hcaptcha.com/siteverify"
	default:
		return "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	}
}
