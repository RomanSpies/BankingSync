package enablebanking

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PEMSource is a function that returns raw PEM-encoded private key bytes.
// It is called fresh for each API request.
type PEMSource func() ([]byte, error)

// AppIDResolver is a function that returns the Enable Banking application ID.
// It is called fresh for each API request to support late-binding configuration.
type AppIDResolver func() (string, error)

// DefaultPEMSource returns a PEMSource that resolves the private key using the
// following priority order:
//  1. "eb_pem_content" setting from the store (uploaded via web UI)
//  2. /data/private.pem file on disk
//  3. Any *.pem file found in /data/
func DefaultPEMSource(getter func(key string) (string, error)) PEMSource {
	return func() ([]byte, error) {
		if getter != nil {
			if content, err := getter("eb_pem_content"); err == nil && content != "" {
				return []byte(content), nil
			}
		}

		const fixed = "/data/private.pem"
		if _, err := os.Stat(fixed); err == nil {
			return os.ReadFile(fixed)
		}

		matches, err := filepath.Glob("/data/*.pem")
		if err != nil || len(matches) == 0 {
			return nil, fmt.Errorf("no PEM key found — upload one via the web UI or mount /data/private.pem")
		}
		return os.ReadFile(matches[0])
	}
}

// DefaultAppIDResolver returns an AppIDResolver that resolves the application ID
// using the following priority order:
//  1. EB_APPLICATION_ID environment variable
//  2. "eb_app_id" setting from the store
//  3. UUID-named *.pem file in /data/ (36-character filename = UUID)
func DefaultAppIDResolver(getter func(key string) (string, error)) AppIDResolver {
	return func() (string, error) {
		if v := os.Getenv("EB_APPLICATION_ID"); v != "" {
			return v, nil
		}
		if getter != nil {
			if id, err := getter("eb_app_id"); err == nil && id != "" {
				return id, nil
			}
		}
		matches, _ := filepath.Glob("/data/*.pem")
		for _, m := range matches {
			base := strings.TrimSuffix(filepath.Base(m), ".pem")
			if len(base) == 36 {
				return base, nil
			}
		}
		return "", fmt.Errorf("Enable Banking Application ID not configured — set EB_APPLICATION_ID or complete web setup")
	}
}
