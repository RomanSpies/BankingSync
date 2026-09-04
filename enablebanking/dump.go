package enablebanking

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
)

const DumpDirEnv = "EB_DUMP_RESPONSES"

var dumpSeq atomic.Uint64

func dumpDir() string { return os.Getenv(DumpDirEnv) }

func dumpResponse(dir, kind string, body []byte) (string, error) {
	if dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}

	var pretty any
	if err := json.Unmarshal(body, &pretty); err != nil {
		return "", fmt.Errorf("capture is JSON-only: %w", err)
	}
	formatted, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format: %w", err)
	}

	name := fmt.Sprintf("%s-%04d.json", kind, dumpSeq.Add(1))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(formatted, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// captureResponse writes a raw response to the dump directory when one is
// configured, logging the outcome. It exists so the three call sites do not each
// repeat the directory check and the same two log lines.
func captureResponse(kind, what string, body []byte) {
	dir := dumpDir()
	if dir == "" {
		return
	}
	path, err := dumpResponse(dir, kind, body)
	if err != nil {
		log.Printf("%s capture failed: %v", what, err)
		return
	}
	if path != "" {
		log.Printf("Captured raw Enable Banking %s to %s", what, path)
	}
}
