package main

import (
	"fmt"
	"log"
	"net/smtp"
	"os"
	"strconv"
)

// sendEmail sends a plain-text notification via SMTP using credentials from
// environment variables. It is a no-op when NOTIFY_EMAIL, SMTP_USER, or
// SMTP_PASS are unset.
func sendEmail(subject, body string) {
	notifyEmail := os.Getenv("NOTIFY_EMAIL")
	smtpUser := os.Getenv("SMTP_USER")
	smtpPass := os.Getenv("SMTP_PASS")
	if notifyEmail == "" || smtpUser == "" || smtpPass == "" {
		return
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

	if err := smtp.SendMail(addr, auth, smtpUser, []string{notifyEmail}, msg); err != nil {
		log.Printf("Failed to send email: %v", err)
		return
	}
	log.Println("Email notification sent")
}
