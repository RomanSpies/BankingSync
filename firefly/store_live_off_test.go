//go:build !fireflylive

package firefly_test

import (
	"os"
	"testing"
)

// TestStoreLive_tagIsSetWhenAnInstanceIsConfigured is the counterpart to the live
// tests in this package, and it is here rather than beside them because it has to
// exist in the build they are missing from.
//
// Dropping -tags fireflylive from the integration job would otherwise leave the
// job running a filter that matches nothing and exiting zero. The same guard sits
// in the root package for the sync harness; both are needed, because each covers
// only its own package's invocation.
func TestStoreLive_tagIsSetWhenAnInstanceIsConfigured(t *testing.T) {
	if url := os.Getenv("FIREFLY_LIVE_URL"); url != "" {
		t.Fatalf("FIREFLY_LIVE_URL is set to %q, so a live run was intended, but this "+
			"binary was built without the fireflylive tag and holds no live store tests; "+
			"add -tags fireflylive to the test invocation", url)
	}
}
