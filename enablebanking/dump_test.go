package enablebanking

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDumpResponse_disabledWhenDirEmpty(t *testing.T) {
	path, err := dumpResponse("", "page", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("dumpResponse: %v", err)
	}
	if path != "" {
		t.Errorf("got %q, want empty path when capture is off", path)
	}
}

func TestDumpResponse_writesFormattedJSON(t *testing.T) {
	dir := t.TempDir()
	path, err := dumpResponse(dir, "page", []byte(`{"transactions":[{"a":1}],"continuation_key":""}`))
	if err != nil {
		t.Fatalf("dumpResponse: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("dump is not valid JSON: %v", err)
	}
	if _, ok := decoded["transactions"]; !ok {
		t.Error("dump lost the transactions key")
	}
	if !strings.Contains(string(raw), "\n  ") {
		t.Error("expected indented output")
	}
}

func TestDumpResponse_createsMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "capture")
	if _, err := dumpResponse(dir, "page", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("dumpResponse: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory not created: %v", err)
	}
}

func TestDumpResponse_rejectsNonJSON(t *testing.T) {
	if _, err := dumpResponse(t.TempDir(), "page", []byte(`not json`)); err == nil {
		t.Fatal("expected an error for non-JSON input")
	}
}

func TestDumpResponse_uniqueFilenamesPerCall(t *testing.T) {
	dir := t.TempDir()
	first, err := dumpResponse(dir, "page", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("first dump: %v", err)
	}
	second, err := dumpResponse(dir, "page", []byte(`{"a":2}`))
	if err != nil {
		t.Fatalf("second dump: %v", err)
	}
	if first == second {
		t.Errorf("both dumps wrote to %q", first)
	}
}

func TestDumpDir_readsEnv(t *testing.T) {
	t.Setenv(DumpDirEnv, "/tmp/capture")
	if got := dumpDir(); got != "/tmp/capture" {
		t.Errorf("got %q, want /tmp/capture", got)
	}
}

func TestDumpDir_emptyWhenUnset(t *testing.T) {
	t.Setenv(DumpDirEnv, "")
	if got := dumpDir(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
