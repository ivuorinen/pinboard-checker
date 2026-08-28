// docs_test.go checks README.md against the code it describes.
//
// Documentation drift was found twice in this repository: the README documented
// a timeout that did not exist, and later described the URL pipeline in the
// order it had *before* a bug fix, so it read as an instruction to reintroduce
// the bug. Both survived review because nothing connected the prose to the
// source. These tests are that connection — they fail when a constant, a flag,
// or a printed label changes without the README following.
package main

import (
	"flag"
	"os"
	"strings"
	"testing"
)

// readmeText returns README.md, failing the test if it cannot be read.
func readmeText(t *testing.T) string {
	t.Helper()

	body, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("failed to read README.md: %v", err)
	}

	return string(body)
}

// Numbers the README states outright must match the constants they describe.
// A reader sizing a run against "up to 5 hops" or "60-second ceiling" is
// relying on these.
func TestREADME_QuotesCurrentConstants(t *testing.T) {
	t.Parallel()

	readme := readmeText(t)

	tests := []struct {
		name   string
		phrase string
		actual string
	}{
		{"rate limit", "1 request per 3 seconds", defaultRateLimit.String()},
		{"backoff ceiling", "60-second ceiling", maxRateLimit.String()},
		{"request timeout", "15 seconds", defaultHTTPTimeout.String()},
		{"per-host interval", "500ms apart", defaultPerHostInterval.String()},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if !strings.Contains(readme, testCase.phrase) {
				t.Errorf("README no longer contains %q; the code says %s",
					testCase.phrase, testCase.actual)
			}
		})
	}

	// Values whose prose form cannot be derived from the constant directly.
	if maxRedirectHops != 5 || !strings.Contains(readme, "up to 5 hops") {
		t.Errorf("maxRedirectHops = %d but README says 'up to 5 hops'", maxRedirectHops)
	}
	if maxRateLimitRetries != 5 || !strings.Contains(readme, "up to 5 attempts") {
		t.Errorf("maxRateLimitRetries = %d but README says 'up to 5 attempts'",
			maxRateLimitRetries)
	}
	if maxSuggestedTags != 9 || !strings.Contains(readme, "up to 9 suggested tags") {
		t.Errorf("maxSuggestedTags = %d but README says 'up to 9 suggested tags'",
			maxSuggestedTags)
	}
	if maxWorkers != 100 || !strings.Contains(readme, "1-100") {
		t.Errorf("maxWorkers = %d but README says the range is 1-100", maxWorkers)
	}
	if !strings.Contains(readme, "`"+autoTagMarker+"`") {
		t.Errorf("README does not mention the %s marker", autoTagMarker)
	}
}

// Every flag the binary defines must appear in the Options list. A flag that
// exists but is undocumented is undiscoverable, and -workers was exactly that
// while a value of 0 could hang the process.
func TestREADME_DocumentsEveryFlag(t *testing.T) {
	t.Parallel()

	readme := readmeText(t)

	// Registered through the same function cli() uses, so this test cannot
	// drift from the real flag set the way a hand-copied list did. The four
	// toggles cli binds to package globals are added here the same way.
	var (
		probe                                     = flag.NewFlagSet("probe", flag.ContinueOnError)
		verboseF, ciF, skipTitlesF, skipAutoTagsF bool
	)

	registerFlags(probe)
	probe.BoolVar(&verboseF, "verbose", false, "")
	probe.BoolVar(&ciF, "ci", false, "")
	probe.BoolVar(&skipTitlesF, "skip-titles", false, "")
	probe.BoolVar(&skipAutoTagsF, "skip-auto-tags", false, "")

	probe.VisitAll(func(f *flag.Flag) {
		if !strings.Contains(readme, "`-"+f.Name) {
			t.Errorf("flag -%s is not documented in README.md", f.Name)
		}
	})
}

// Both environment variables the program reads must be documented in the README
// and present in .env.example, or a user cannot discover them.
func TestREADME_DocumentsEnvironment(t *testing.T) {
	t.Parallel()

	readme := readmeText(t)

	example, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatalf("failed to read .env.example: %v", err)
	}

	for _, name := range []string{tokenName, bitlyTokenName} {
		if !strings.Contains(readme, name) {
			t.Errorf("%s is not documented in README.md", name)
		}
		if !strings.Contains(string(example), name) {
			t.Errorf("%s is missing from .env.example", name)
		}
	}
}

// The statistics section names two counters by the label the report prints. If
// a label is reworded, the README must be reworded with it.
//
//nolint:paralleltest // captureOutput swaps the global os.Stdout
func TestREADME_MatchesPrintedCounterLabels(t *testing.T) {
	readme := readmeText(t)

	report := newStatistics()
	report.TotalBookmarks = 1
	report.Skipped = 1
	report.ApplyErrors = 1
	report.UpdatesPerformed = 1

	printed := captureOutput(report.Print)

	for _, label := range []string{"Skipped (unchecked)", "Writes rejected"} {
		if !strings.Contains(printed, label) {
			t.Errorf("the report no longer prints %q", label)
		}
		if !strings.Contains(readme, label) {
			t.Errorf("README does not document the %q counter", label)
		}
	}
}

// Claims the README used to make and must not make again. Each one described
// behavior the code did not have; the second is the ordering bug that deleted
// bookmarks the parenthesis repair was meant to fix.
func TestREADME_HasNoRetiredClaims(t *testing.T) {
	t.Parallel()

	readme := readmeText(t)

	retired := map[string]string{
		"Checks if the URL is accessible": "the liveness check runs after the repair steps, not before",
		"2xx/3xx":                         "only 404 and 410 are treated as dead",
		"See LICENSE file for details":    "the README names the license directly",
		// The blanket 4xx rule deleted bookmarks over 429 and 408, which are the
		// server throttling this tool, not the page being gone.
		"4xx errors (404, 410, etc.)": "only 404 and 410 delete; every other 4xx is skipped",
		"a definite 4xx":              "only 404 and 410 delete; every other 4xx is skipped",
	}

	for phrase, why := range retired {
		if strings.Contains(readme, phrase) {
			t.Errorf("README contains retired claim %q: %s", phrase, why)
		}
	}

	// A hardcoded Go version reintroduces the drift the go.mod pointer removed.
	if strings.Contains(readme, "Go 1.2") {
		t.Error("README hardcodes a Go version; point at the go directive in go.mod instead")
	}
}
