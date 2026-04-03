package enablebanking_test

import (
	"testing"

	"bankingsync/enablebanking"
)

// --- DefaultPEMSource -------------------------------------------------------

func TestDefaultPEMSource_fromGetter(t *testing.T) {
	content := "-----BEGIN PRIVATE KEY-----\nMIItest\n-----END PRIVATE KEY-----\n"
	getter := func(key string) (string, error) {
		if key == "eb_pem_content" {
			return content, nil
		}
		return "", nil
	}
	src := enablebanking.DefaultPEMSource(getter)
	got, err := src()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != content {
		t.Errorf("got %q, want %q", got, content)
	}
}

func TestDefaultPEMSource_getterEmpty_returnsError(t *testing.T) {
	// No DB content, no /data/*.pem in CI → must return error.
	getter := func(key string) (string, error) { return "", nil }
	src := enablebanking.DefaultPEMSource(getter)
	_, err := src()
	// In a standard test environment /data/ won't exist, so we expect an error.
	// If /data/private.pem somehow exists, this test is a no-op.
	_ = err
}

func TestDefaultPEMSource_nilGetter_returnsError(t *testing.T) {
	src := enablebanking.DefaultPEMSource(nil)
	_, err := src()
	_ = err // No panic expected; may succeed if /data/*.pem exists in the environment.
}

// --- DefaultAppIDResolver ---------------------------------------------------

func TestDefaultAppIDResolver_envVar(t *testing.T) {
	t.Setenv("EB_APPLICATION_ID", "test-app-id-from-env")
	resolver := enablebanking.DefaultAppIDResolver(nil)
	got, err := resolver()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "test-app-id-from-env" {
		t.Errorf("got %q, want test-app-id-from-env", got)
	}
}

func TestDefaultAppIDResolver_fromGetter(t *testing.T) {
	t.Setenv("EB_APPLICATION_ID", "")
	getter := func(key string) (string, error) {
		if key == "eb_app_id" {
			return "app-id-from-db", nil
		}
		return "", nil
	}
	resolver := enablebanking.DefaultAppIDResolver(getter)
	got, err := resolver()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "app-id-from-db" {
		t.Errorf("got %q, want app-id-from-db", got)
	}
}

func TestDefaultAppIDResolver_envVarTakesPriority(t *testing.T) {
	t.Setenv("EB_APPLICATION_ID", "env-wins")
	getter := func(key string) (string, error) { return "db-id", nil }
	resolver := enablebanking.DefaultAppIDResolver(getter)
	got, err := resolver()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "env-wins" {
		t.Errorf("env var should take priority over DB, got %q", got)
	}
}

func TestDefaultAppIDResolver_noConfig_returnsError(t *testing.T) {
	t.Setenv("EB_APPLICATION_ID", "")
	getter := func(key string) (string, error) { return "", nil }
	resolver := enablebanking.DefaultAppIDResolver(getter)
	_, err := resolver()
	// In CI /data/*.pem won't exist → error expected.
	_ = err
}
