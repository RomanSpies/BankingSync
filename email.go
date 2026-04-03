package main

import (
	"context"
	"fmt"
	"log"
	"net/smtp"
	"os"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// trySendEmail sends a plain-text email and returns any error.
func trySendEmail(ctx context.Context, subject, body string) error {
	tracer := otel.Tracer("bankingsync")
	_, span := tracer.Start(ctx, "email.send",
		trace.WithAttributes(attribute.String("subject", subject)),
	)
	defer span.End()

	notifyEmail := os.Getenv("NOTIFY_EMAIL")
	smtpUser := os.Getenv("SMTP_USER")
	smtpPass := os.Getenv("SMTP_PASS")
	if notifyEmail == "" || smtpUser == "" || smtpPass == "" {
		err := fmt.Errorf("email not configured: NOTIFY_EMAIL, SMTP_USER, and SMTP_PASS are required")
		span.SetAttributes(attribute.Bool("configured", false))
		return err
	}
	span.SetAttributes(attribute.Bool("configured", true))

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
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// sendEmail sends a plain-text notification via SMTP. It is a no-op when email
// is not configured and logs errors silently.
func sendEmail(ctx context.Context, subject, body string) {
	if err := trySendEmail(ctx, subject, body); err != nil {
		if os.Getenv("NOTIFY_EMAIL") != "" {
			log.Printf("Failed to send email: %v", err)
		}
		return
	}
	log.Println("Email notification sent")
}

// sendTestEmail sends a test email and returns any error.
func sendTestEmail(ctx context.Context) error {
	return trySendEmail(ctx,
		"BankingSync: Test Email",
		"This is a test email from BankingSync. If you received this, your email configuration is working correctly.",
	)
}
