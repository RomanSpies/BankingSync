package main

import (
	"fmt"
	"log"
	"net/smtp"
	"os"
	"strconv"
)

// trySendEmail sends a plain-text email and returns any error.
func trySendEmail(subject, body string) error {
	notifyEmail := os.Getenv("NOTIFY_EMAIL")
	smtpUser := os.Getenv("SMTP_USER")
	smtpPass := os.Getenv("SMTP_PASS")
	if notifyEmail == "" || smtpUser == "" || smtpPass == "" {
		return fmt.Errorf("email not configured: NOTIFY_EMAIL, SMTP_USER, and SMTP_PASS are required")
	}

	smtpHost := os.Getenv("SMTP_HOST")
	if smtpHost == "" {
		smtpHost = "smtp.gmail.com"
	}
	smtpPort := 587
	if p := os.Getenv("SMTP_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			smtpPort = n
		}
	}

	addr := fmt.Sprintf("%s:%d", smtpHost, smtpPort)
	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)

	msg := []byte(fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		smtpUser, notifyEmail, subject, body,
	))

	return smtp.SendMail(addr, auth, smtpUser, []string{notifyEmail}, msg)
}

// sendEmail sends a plain-text notification via SMTP. It is a no-op when email
// is not configured and logs errors silently.
func sendEmail(subject, body string) {
	if err := trySendEmail(subject, body); err != nil {
		if os.Getenv("NOTIFY_EMAIL") != "" {
			log.Printf("Failed to send email: %v", err)
		}
		return
	}
	log.Println("Email notification sent")
}

// sendTestEmail sends a test email and returns any error.
func sendTestEmail() error {
	return trySendEmail(
		"BankingSync: Test Email",
		"This is a test email from BankingSync. If you received this, your email configuration is working correctly.",
	)
}
