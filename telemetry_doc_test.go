package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The telemetry reference is a document somebody builds alerts from, and an
// alert written against a series that no longer exists fails silently — it just
// never fires. So the document is checked against the source rather than
// maintained alongside it and hoped about.
//
// This is the same reasoning that puts budget/sensitivity.md behind a test. The
// difference is that sensitivity.md is generated and this is written, because
// what a series *means* cannot be generated and is most of the document's value.
// So the test checks that every instrument is mentioned, and leaves whether the
// sentence about it is true to a reader.
const telemetryDoc = "docs/telemetry.md"

var (
	instrumentRE = regexp.MustCompile(`(?:Counter|Histogram|ObservableGauge)\("(bankingsync_\w+)"`)
	spanRE       = regexp.MustCompile(`\.Start\((?:ctx|req\.Context\(\)|r\.Context\(\)|\w+),\s*"([a-z_.]+)"`)
	logRE        = regexp.MustCompile(`olog\.(?:Info|Warn|Error)\([^,]+,\s*"([a-z_.]+)"`)
	histogramRE  = regexp.MustCompile(`Float64Histogram\("(bankingsync_\w+)"`)
	boundariesRE = regexp.MustCompile(`WithExplicitBucketBoundaries\(([^)]*)\)`)
)

// maxSourceFiles bounds what the walk below is willing to call this repository.
//
// It exists because the failure it guards against is silence rather than an
// error. CI sets GOMODCACHE inside the project directory, so an unfiltered walk
// descends into the module cache: 49353 Go files instead of 126, several of them
// machine-translated C of eight to ten megabytes. Nothing fails — the regular
// expressions simply run over four hundred times the intended input, and the
// package times out after ten minutes with a stack in the middle of a match.
// A test that is wrong should say so in a second, not eight minutes later in
// somebody else's pipeline.
const maxSourceFiles = 500

// sourceFiles is every non-test Go file in this repository.
//
// Directories beginning with a dot or an underscore are skipped, and so is
// testdata, which is the rule cmd/go itself applies when deciding what belongs
// to a module. Here it matters more than convention: the module cache and the
// build cache both live under .cache in CI, and .git is walked otherwise too.
func sourceFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if name := info.Name(); path != "." &&
				(strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(out) < 20 {
		t.Fatalf("only %d source files found; the walk is not reaching the repository", len(out))
	}
	if len(out) > maxSourceFiles {
		t.Fatalf("%d source files found, which is more than this repository has; the walk "+
			"has left it and is reading a dependency tree or a cache", len(out))
	}
	return out
}

func readTelemetryDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(telemetryDoc)
	if err != nil {
		t.Fatalf("read %s: %v", telemetryDoc, err)
	}
	return string(b)
}

// TestTelemetryDoc_namesEveryInstrument is the one that matters most. A metric
// that exists and is undocumented is a metric nobody will build a panel from.
func TestTelemetryDoc_namesEveryInstrument(t *testing.T) {
	doc := readTelemetryDoc(t)

	found := map[string]string{}
	for path, src := range sourceFiles(t) {
		for _, m := range instrumentRE.FindAllStringSubmatch(src, -1) {
			found[m[1]] = path
		}
	}
	if len(found) < 30 {
		t.Fatalf("only %d instruments found in the source; the pattern is not matching", len(found))
	}

	var missing []string
	for name, path := range found {
		if !strings.Contains(doc, "`"+name+"`") {
			missing = append(missing, name+" ("+path+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d instruments are not named in %s:\n  %s\n\nAn alert cannot be written "+
			"against a series nobody has written down, and a series nobody has written "+
			"down is one that will be removed without anybody noticing.",
			len(missing), telemetryDoc, strings.Join(missing, "\n  "))
	}
	t.Logf("%d instruments, all documented", len(found))
}

// TestTelemetryDoc_quotesTheRealBucketBoundaries keeps the part of the document
// a query is written directly against from drifting.
//
// Bucket edges are not decoration: a dashboard computing "the share of decisions
// between the two thresholds" selects on le="0.5" and le="0.9", and those
// selectors are silently wrong the day an edge moves. So the document has to
// carry the real list, verbatim.
func TestTelemetryDoc_quotesTheRealBucketBoundaries(t *testing.T) {
	// Backticks and line breaks are markdown, not meaning: a bucket list that
	// happens to wrap is still quoted. Both are flattened before comparing.
	doc := strings.Join(strings.Fields(strings.ReplaceAll(readTelemetryDoc(t), "`", "")), " ")

	checked := 0
	for _, src := range sourceFiles(t) {
		// Each declaration is matched to the boundaries that follow it, stopping
		// at the next declaration so that a histogram without explicit buckets
		// cannot borrow the next one's.
		decls := histogramRE.FindAllStringSubmatchIndex(src, -1)
		for i, d := range decls {
			end := len(src)
			if i+1 < len(decls) {
				end = decls[i+1][0]
			}
			name := src[d[2]:d[3]]
			m := boundariesRE.FindStringSubmatch(src[d[1]:end])
			if m == nil {
				t.Errorf("%s declares no explicit bucket boundaries; the defaults are "+
					"latency-shaped and wrong for everything this program measures", name)
				continue
			}
			parts := strings.Split(m[1], ",")
			for j := range parts {
				parts[j] = strings.TrimSpace(parts[j])
			}
			want := strings.Join(parts, ", ")
			if !strings.Contains(doc, want) {
				t.Errorf("%s has boundaries %s and the document does not quote them",
					name, want)
			}
			checked++
		}
	}
	if checked < 5 {
		t.Fatalf("only %d histograms checked; the pattern is not matching", checked)
	}
	t.Logf("%d histograms, all boundaries quoted", checked)
}

// TestTelemetryDoc_namesEverySpanAndLogRecord covers the other two signals.
//
// Log record names are allowed to appear abbreviated — the document groups them
// by area and writes `.completed` under `actual` rather than repeating the
// prefix on every row — so both forms are accepted.
func TestTelemetryDoc_namesEverySpanAndLogRecord(t *testing.T) {
	doc := readTelemetryDoc(t)
	src := sourceFiles(t)

	documented := func(name string) bool {
		if strings.Contains(doc, name) {
			return true
		}
		area, rest, ok := strings.Cut(name, ".")
		return ok && strings.Contains(doc, "`."+rest+"`") && strings.Contains(doc, "`"+area+"`")
	}

	for what, re := range map[string]*regexp.Regexp{"span": spanRE, "log record": logRE} {
		found := map[string]bool{}
		for _, s := range src {
			for _, m := range re.FindAllStringSubmatch(s, -1) {
				found[m[1]] = true
			}
		}
		if len(found) < 10 {
			t.Fatalf("only %d %ss found; the pattern is not matching", len(found), what)
		}
		var missing []string
		for name := range found {
			if !documented(name) {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%d %ss are not named in %s:\n  %s",
				len(missing), what, telemetryDoc, strings.Join(missing, "\n  "))
		}
		t.Logf("%d %ss, all documented", len(found), what)
	}
}
