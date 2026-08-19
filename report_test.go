// report_test.go asserts what the statistics block actually renders. The report
// is the tool's only output, so every conditional line in it is checked against
// the counters that produce it rather than merely executed.
package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// captureStderr collects everything written to os.Stderr while emit runs. The
// diagnostics that -verbose emits go there rather than to stdout, so they stay
// out of the statistics block.
func captureStderr(t *testing.T, emit func()) string {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}

	previous := os.Stderr
	os.Stderr = write

	done := make(chan string, 1)

	go func() {
		var buf strings.Builder
		buf.Grow(1024)

		chunk := make([]byte, 1024)

		for {
			n, readErr := read.Read(chunk)
			if n > 0 {
				buf.Write(chunk[:n])
			}
			if readErr != nil {
				break
			}
		}

		done <- buf.String()
	}()

	emit()

	os.Stderr = previous

	if err := write.Close(); err != nil {
		t.Errorf("failed to close pipe: %v", err)
	}

	return <-done
}

// A fully populated report must render every conditional line. Each of these
// counters was added because a run could otherwise finish without saying what
// it had done.
//
//nolint:paralleltest // captureOutput swaps the global os.Stdout
func TestPrint_RendersEveryConditionalLine(t *testing.T) {
	report := newStatistics()
	report.TotalBookmarks = 10
	report.ActionNoChange = 4
	report.ActionUpdate = 3
	report.ActionDelete = 1
	report.Skipped = 2
	report.URLsChanged = 1
	report.TitlesAdded = 2
	report.TagsAdded = 3
	report.ParenthesesFixed = 1
	report.Redirects = 2
	report.TitlesFetched = 2
	report.TitlesFailed = 1
	report.TagsFetched = 3
	report.TagsFailed = 2
	report.UpdatesPerformed = 3
	report.DeletionsPerformed = 1
	report.ApplyErrors = 2
	report.ShortURLsExpanded[svcBitly] = 1
	report.ShortURLsFailed[svcTinyURL] = 2
	report.ShortURLsExpanded[svcIsGd] = 3
	report.StatusCodes[opURLValidation] = map[int]int{200: 5, 404: 1}

	output := captureOutput(report.Print)

	for _, want := range []string{
		"Skipped (unchecked):2",
		"Tags added:         3",
		"URLs changed:       1",
		"Titles added:       2",
		"Parentheses fixed:  1",
		"Redirects followed: 2",
		"bit.ly:         1 success, 0 failed",
		"tinyurl:        0 success, 2 failed",
		"is.gd:          3 success, 0 failed",
		"Deletions performed: 1",
		"Writes rejected:    2",
		// printFetchStats renders the failure count only when there is one.
		"Titles fetched:     2 (1 failed)",
		"Tags fetched:       3 (2 failed)",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("report is missing %q\n--- output ---\n%s", want, output)
		}
	}
}

// A status code net/http has no text for must not render an empty label.
//
//nolint:paralleltest // captureOutput swaps the global os.Stdout
func TestPrint_UnknownStatusCode(t *testing.T) {
	report := newStatistics()
	report.TotalBookmarks = 1
	report.StatusCodes[opTitleFetch] = map[int]int{599: 1}

	output := captureOutput(report.Print)

	if !strings.Contains(output, "599 Unknown: 1") {
		t.Errorf("report is missing the unknown-status line\n--- output ---\n%s", output)
	}
}

// The shortener section is skipped entirely when no shortener ran, so a report
// for an account with no short links carries no empty heading.
//
//nolint:paralleltest // captureOutput swaps the global os.Stdout
func TestPrint_OmitsShortenerSectionWhenUnused(t *testing.T) {
	report := newStatistics()
	report.TotalBookmarks = 1
	report.Redirects = 1

	output := captureOutput(report.Print)

	if strings.Contains(output, "Short URLs expanded") {
		t.Errorf("report shows an empty shortener section\n--- output ---\n%s", output)
	}
	if !strings.Contains(output, "Redirects followed: 1") {
		t.Errorf("report is missing the redirect line\n--- output ---\n%s", output)
	}
}

// Sorted iteration means the same counters render identically every time. The
// report used to range over maps directly, so two runs could not be diffed.
//
//nolint:paralleltest // captureOutput swaps the global os.Stdout
func TestPrint_IsDeterministic(t *testing.T) {
	build := func() *Statistics {
		report := newStatistics()
		report.TotalBookmarks = 3
		report.StatusCodes[opURLValidation] = map[int]int{200: 1, 301: 2, 404: 3}
		report.StatusCodes[opTitleFetch] = map[int]int{200: 4, 500: 5}
		report.StatusCodes[opTagFetch] = map[int]int{200: 6}

		return report
	}

	first := captureOutput(build().Print)
	for range 5 {
		if got := captureOutput(build().Print); got != first {
			t.Fatalf("report differs between runs\n--- first ---\n%s\n--- later ---\n%s",
				first, got)
		}
	}

	// Ordering is the property under test, so pin it explicitly.
	titleAt := strings.Index(first, "Title Fetch")
	urlAt := strings.Index(first, "URL Validation")
	if titleAt < 0 || urlAt < 0 || titleAt > urlAt {
		t.Errorf("operations are not in sorted order\n--- output ---\n%s", first)
	}
	if strings.Index(first, "200 OK") > strings.Index(first, "500 Internal Server Error") {
		t.Errorf("status codes are not in ascending order\n--- output ---\n%s", first)
	}
}

// -verbose diagnostics must name the bookmark and the reason, and must go to
// stderr so they cannot interleave with the statistics block on stdout.
//
//nolint:paralleltest // mutates the verbose flag and the stats singleton
func TestVerbose_SkipDiagnosticNamesURLAndReason(t *testing.T) {
	resetStats(t)
	setVerbose(t, true)

	var action BookmarkAction

	output := captureStderr(t, func() {
		action = recordSkip(Bookmark{URL: testURL}, errors.New("scheme not dialable"))
	})

	if action.Action != ActionNoChange {
		t.Errorf("recordSkip() action = %v, want ActionNoChange", action.Action)
	}
	if !strings.Contains(output, testURL) {
		t.Errorf("stderr = %q, want it to name the bookmark", output)
	}
	if !strings.Contains(output, "scheme not dialable") {
		t.Errorf("stderr = %q, want it to carry the reason", output)
	}
}

//nolint:paralleltest // mutates the verbose flag and the stats singleton
func TestVerbose_ApplyErrorNamesOperationAndURL(t *testing.T) {
	setVerbose(t, true)

	output := captureStderr(t, func() {
		reportApplyError("deleting", testURL, errors.New("boom"))
	})

	for _, want := range []string{"deleting", testURL, "boom"} {
		if !strings.Contains(output, want) {
			t.Errorf("stderr = %q, want it to contain %q", output, want)
		}
	}
}

// A failed expansion is reported but not fatal, and the message must say which
// service could not resolve the link.
//
//nolint:paralleltest // mutates the verbose flag, the shortener seam, and stats
func TestVerbose_ShortenerFailureNamesService(t *testing.T) {
	resetStats(t)
	setVerbose(t, true)
	swap(t, &unshortenIsGd, func(_ string) (string, error) {
		return "", errors.New("upstream refused")
	})

	var got string

	output := captureStderr(t, func() {
		result, err := expandURL("https://is.gd/abc", true)
		if err != nil {
			t.Errorf("expandURL() error = %v, want nil", err)
		}
		got = result
	})

	if got != "https://is.gd/abc" {
		t.Errorf("expandURL() = %q, want the original URL", got)
	}
	if !strings.Contains(output, "is.gd") {
		t.Errorf("stderr = %q, want it to name the service", output)
	}
	if !strings.Contains(output, "upstream refused") {
		t.Errorf("stderr = %q, want it to carry the cause", output)
	}
}

// The progress bar is built only outside CI mode. Its absence must not change
// the actions produced.
//
//nolint:paralleltest // mutates the mode flags and the stats singleton
func TestProcessBookmarksParallel_ProgressBarDoesNotChangeResults(t *testing.T) {
	resetStats(t)
	setSkipFlags(t, true, true)
	setCIMode(t, false)

	server := staticServer(t, http.StatusOK, "")

	bookmarks := make([]Bookmark, 3)
	for i := range bookmarks {
		bookmarks[i] = Bookmark{URL: server.URL, Title: "t", Tags: "x"}
	}

	// The bar writes to stderr; capturing keeps the test output readable.
	var actions []BookmarkAction

	captureStderr(t, func() {
		actions = processBookmarksParallel(bookmarks, testToken, 2)
	})

	if len(actions) != len(bookmarks) {
		t.Errorf("got %d actions, want %d", len(actions), len(bookmarks))
	}
}

// The -verbose narration on stdout must name the bookmark at every stage it
// reports, so a run can be followed without guessing which URL a line refers to.
//
//nolint:paralleltest // mutates the verbose and skip flags and the stats singleton
func TestVerbose_NarratesProcessingToStdout(t *testing.T) {
	resetStats(t)
	setVerbose(t, true)
	setSkipFlags(t, false, false)

	page := staticServer(t, http.StatusOK,
		"<html><head><title>Fetched Page</title></head></html>")

	suggest := staticServer(t, http.StatusOK, `[{"popular":["go"]}]`)
	useEndpoint(t, suggest.URL)

	var action BookmarkAction

	output := captureOutput(func() {
		action = processBookmark(Bookmark{URL: page.URL}, testToken)
	})

	if action.Action != ActionUpdate {
		t.Fatalf("processBookmark() action = %v, want ActionUpdate", action.Action)
	}

	for _, want := range []string{
		"Processing: " + page.URL,
		"Fetched title: Fetched Page",
		"Fetched tags: go," + autoTagMarker,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("stdout is missing %q\n--- output ---\n%s", want, output)
		}
	}
}

// The parenthesis repair narrates both the attempt and the result, naming the
// URL on each side of the rewrite.
//
//nolint:paralleltest // mutates the verbose flag and the stats singleton
func TestVerbose_NarratesParenthesisRepair(t *testing.T) {
	resetStats(t)
	setVerbose(t, true)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ")") {
				w.WriteHeader(http.StatusNotFound)

				return
			}
			w.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(server.Close)

	withParen := server.URL + "/page)"

	var got string

	output := captureOutput(func() {
		result, err := fixParenthesesSuffix(withParen, true)
		if err != nil {
			t.Errorf("fixParenthesesSuffix() error = %v, want nil", err)
		}
		got = result
	})

	if got != server.URL+"/page" {
		t.Errorf("fixParenthesesSuffix() = %q, want the trimmed URL", got)
	}
	if !strings.Contains(output, "ends with ')'") {
		t.Errorf("stdout = %q, want the retry notice", output)
	}
	if !strings.Contains(output, withParen+"' -> '"+server.URL+"/page") {
		t.Errorf("stdout = %q, want both sides of the rewrite", output)
	}
}

//nolint:paralleltest // mutates the verbose flag and the stats singleton
func TestVerbose_NarratesRedirect(t *testing.T) {
	resetStats(t)
	setVerbose(t, true)

	final := staticServer(t, http.StatusOK, "")

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", final.URL)
			w.WriteHeader(http.StatusMovedPermanently)
		}))
	t.Cleanup(server.Close)

	output := captureOutput(func() {
		if _, err := followRedirects(server.URL, true); err != nil {
			t.Errorf("followRedirects() error = %v, want nil", err)
		}
	})

	if !strings.Contains(output, final.URL) {
		t.Errorf("stdout = %q, want it to name the redirect destination", output)
	}
}

// The 429 notice must say which endpoint was throttled and what the interval
// became, so a slow run is diagnosable.
//
//nolint:paralleltest // mutates the verbose flag and drives the rate limiter
func TestVerbose_NarratesRateLimitBackoff(t *testing.T) {
	resetStats(t)
	setVerbose(t, true)
	swap(t, &rateLimitInterval, time.Nanosecond)

	t.Cleanup(func() { rateLimiter.Reset(time.Millisecond) })

	attempts := 0
	server := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, `[]`)
	})
	useEndpoint(t, server.URL)

	output := captureStderr(t, func() {
		if _, err := getBookmarks(testToken); err != nil {
			t.Errorf("getBookmarks() error = %v, want nil", err)
		}
	})

	if !strings.Contains(output, "/posts/all") {
		t.Errorf("stderr = %q, want it to name the throttled endpoint", output)
	}
	if !strings.Contains(output, "interval now") {
		t.Errorf("stderr = %q, want it to report the widened interval", output)
	}
}

// A malformed endpoint must fail while the request is being built rather than
// producing a request that goes somewhere unintended.
//
//nolint:paralleltest // uses t.Setenv and repoints bitlyEndpoint
func TestUnshortenBitlyImpl_RejectsUnbuildableRequest(t *testing.T) {
	t.Setenv(bitlyTokenName, "bitly-token")
	// A control character makes http.NewRequest reject the URL.
	useBitlyEndpoint(t, "http://exa\x7fmple.com")

	if _, err := unshortenBitlyImpl(testBitlyURL); err == nil {
		t.Error("unshortenBitlyImpl() error = nil, want a request-build failure")
	}
}

// A transport error that is not a *url.Error still gets a usable message.
func TestSanitizeTransportError_NonURLError(t *testing.T) {
	t.Parallel()

	err := sanitizeTransportError("/posts/all", errors.New("plain failure"))
	if !strings.Contains(err.Error(), "/posts/all") {
		t.Errorf("error = %v, want it to name the path", err)
	}
	if !strings.Contains(err.Error(), "plain failure") {
		t.Errorf("error = %v, want it to carry the cause", err)
	}
}
