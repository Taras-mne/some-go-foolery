// Package email sends transactional emails via SMTP (STARTTLS).
package email

import (
	"fmt"
	"net/smtp"
)

// Config holds SMTP connection parameters read from environment variables.
type Config struct {
	Host    string // SMTP_HOST — e.g. smtp.gmail.com
	Port    string // SMTP_PORT — e.g. 587
	User    string // SMTP_USER — auth username / sender address
	Pass    string // SMTP_PASS — app password or SMTP password
	From    string // SMTP_FROM — display name + address, e.g. "Claudy <noreply@example.com>"
	BaseURL string // BASE_URL  — public relay URL, e.g. https://relay.example.com
}

// Enabled reports whether all required SMTP fields are present.
func (c Config) Enabled() bool {
	return c.Host != "" && c.Port != "" && c.User != "" && c.Pass != ""
}

// SendVerification emails a verification link to addr.
func SendVerification(cfg Config, to, token string) error {
	if !cfg.Enabled() {
		return fmt.Errorf("SMTP not configured")
	}

	from := cfg.From
	if from == "" {
		from = cfg.User
	}

	link := cfg.BaseURL + "/auth/verify?token=" + token

	body := "Hello!\r\n\r\n" +
		"Click the link below to verify your Claudy account:\r\n\r\n" +
		"  " + link + "\r\n\r\n" +
		"This link expires in 24 hours.\r\n" +
		"If you did not sign up for Claudy, you can ignore this email.\r\n"

	msg := []byte(
		"From: " + from + "\r\n" +
			"To: " + to + "\r\n" +
			"Subject: Verify your Claudy account\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=UTF-8\r\n" +
			"\r\n" +
			body,
	)

	auth := smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)
	return smtp.SendMail(cfg.Host+":"+cfg.Port, auth, cfg.User, []string{to}, msg)
}
