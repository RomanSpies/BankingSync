//go:build !fireflylive

package main

import (
	"os"
	"testing"
)

// liveBackends is empty unless the fireflylive build tag is set.
//
// The tag, not an environment variable, is what makes it impossible for a plain
// `go test ./...` to reach a real Firefly instance: without the tag the live
// harness is not compiled at all, so there is no code path to reach by accident.
func liveBackends() []backendCase { return nil }

// TestFireflyLive_tagIsSetWhenAnInstanceIsConfigured is the other half of the
// gate, and it lives here rather than in live_on_test.go on purpose.
//
// Its counterpart TestFireflyLive_backendIsRegistered can only catch a live
// backend that is compiled but unreachable. It cannot catch `-tags fireflylive`
// being dropped from the CI job, because then it is not compiled either — the
// job would filter for a subtest that does not exist, match nothing, and exit
// zero. A green pipeline that ran no integration test at all is the single worst
// outcome this arrangement is built to prevent, so the check has to sit in the
// build that would be left behind.
//
// The environment variable is the signal that a live run was intended: it is set
// only by the integration job.
func TestFireflyLive_tagIsSetWhenAnInstanceIsConfigured(t *testing.T) {
	if url := os.Getenv("FIREFLY_LIVE_URL"); url != "" {
		t.Fatalf("FIREFLY_LIVE_URL is set to %q, so a live run was intended, but this "+
			"binary was built without the fireflylive tag and contains no live backend; "+
			"add -tags fireflylive to the test invocation", url)
	}
}
