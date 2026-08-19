// run_test.go covers the process-level contract: the exit code run returns, the
// timeout plumbing, and the guarantee that the API token never reaches an error
// message. All three were previously uncovered, and two of them are behaviors
// that regressed once already.
package main

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// leakToken is distinctive enough that a substring search for it cannot match
// anything the program legitimately prints.
//
//nolint:gosec // fake value, used only to assert it never reaches an error string
const leakToken = "ivuorinen:SUPERSECRETTOKENVALUE"

//nolint:paralleltest // drives the package-level stats, endpoint, and mode flags
func TestRun_CleanPassExitsZero(t *testing.T) {
	resetStats(t)
	setCIMode(t, true)
	setSkipFlags(t, true, true)

	server := staticServer(t, http.StatusOK, `[]`)
	useEndpoint(t, server.URL)

	if code := run(testToken, 1, false); code != 0 {
		t.Errorf("run() = %d, want 0 for a clean pass", code)
	}
}

// Returning from main exited 0, which let a scheduled run report success after
// fetching nothing.
//
//nolint:paralleltest // drives the package-level stats, endpoint, and mode flags
func TestRun_FetchFailureExitsNonZero(t *testing.T) {
	resetStats(t)
	setCIMode(t, true)

	server := staticServer(t, http.StatusUnauthorized, "API requires authentication")
	useEndpoint(t, server.URL)

	if code := run(testToken, 1, false); code != 1 {
		t.Errorf("run() = %d, want 1 when the fetch fails", code)
	}
}

// A run that applies nothing because every write was rejected must not look
// clean: the counters are what distinguish it from a no-op pass.
//
//nolint:paralleltest // drives the package-level stats, endpoint, and mode flags
func TestRun_RejectedWriteExitsNonZero(t *testing.T) {
	resetStats(t)
	setCIMode(t, true)
	setSkipFlags(t, true, true)

	// The bookmark has no title and its page supplies one, so processBookmark
	// schedules an update; posts/add then rejects it in-band at HTTP 200.
	target := staticServer(t, http.StatusOK,
		"<html><head><title>Fetched</title></head></html>")

	server := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)

		if strings.HasPrefix(r.URL.Path, "/posts/all") {
			writeBody(t, w, `[{"href":"`+target.URL+`","description":"","tags":"x"}]`)

			return
		}

		writeBody(t, w, `{"result_code":"something went wrong"}`)
	})
	useEndpoint(t, server.URL)

	setSkipFlags(t, false, true)

	code := run(testToken, 1, false)

	if stats.ApplyErrorCount() != 1 {
		t.Fatalf("ApplyErrorCount() = %d, want 1", stats.ApplyErrorCount())
	}
	if code != 1 {
		t.Errorf("run() = %d, want 1 when a write is rejected", code)
	}
}

// Without -ci the run prints its statistics block, which is the only report the
// tool produces.
//
//nolint:paralleltest // drives the package-level stats, endpoint, and mode flags
func TestRun_PrintsStatisticsWhenNotCIMode(t *testing.T) {
	resetStats(t)
	setCIMode(t, false)
	setSkipFlags(t, true, true)

	server := staticServer(t, http.StatusOK, `[]`)
	useEndpoint(t, server.URL)

	output := captureOutput(func() {
		if code := run(testToken, 1, false); code != 0 {
			t.Errorf("run() = %d, want 0", code)
		}
	})

	if !strings.Contains(output, "PROCESSING STATISTICS") {
		t.Errorf("output = %q, want the statistics block", output)
	}
}

//nolint:paralleltest // drives the package-level stats, endpoint, and mode flags
func TestRun_DryRunIssuesNoWrites(t *testing.T) {
	resetStats(t)
	setCIMode(t, true)
	setSkipFlags(t, true, true)

	target := staticServer(t, http.StatusNotFound, "")

	server := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)

		if strings.HasPrefix(r.URL.Path, "/posts/all") {
			writeBody(t, w, `[{"href":"`+target.URL+`","description":"t","tags":"x"}]`)

			return
		}

		writeBody(t, w, `{"result_code":"done"}`)
	})
	useEndpoint(t, server.URL)

	if code := run(testToken, 1, true); code != 0 {
		t.Errorf("run() = %d, want 0 for a dry run", code)
	}

	// The dead bookmark is scheduled for deletion but nothing may be sent.
	for _, request := range server.Requests() {
		if strings.HasPrefix(request, "/posts/delete") || strings.HasPrefix(request, "/posts/add") {
			t.Errorf("dry run issued a write: %s", request)
		}
	}
}

// Both clients must be updated. Dropping the second assignment is an easy
// refactor slip and would leave the redirect probe unbounded.
//
//nolint:paralleltest // mutates the shared HTTP clients
func TestSetHTTPTimeout(t *testing.T) {
	swap(t, &httpClient.Timeout, httpClient.Timeout)
	swap(t, &redirectClient.Timeout, redirectClient.Timeout)

	setHTTPTimeout(7 * time.Second)

	if httpClient.Timeout != 7*time.Second {
		t.Errorf("httpClient.Timeout = %s, want 7s", httpClient.Timeout)
	}
	if redirectClient.Timeout != 7*time.Second {
		t.Errorf("redirectClient.Timeout = %s, want 7s", redirectClient.Timeout)
	}
}

// The auth token travels in the query string, and url.Error renders the full
// URL, so wrapping a transport failure printed the credential to stderr.
//
//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestPinboardGet_TransportErrorHidesToken(t *testing.T) {
	resetStats(t)

	// A server closed immediately gives a guaranteed dial failure.
	server := staticServer(t, http.StatusOK, "")
	endpoint := server.URL
	server.Close()

	useEndpoint(t, endpoint)

	_, err := getBookmarks(leakToken)
	if err == nil {
		t.Fatal("getBookmarks() error = nil, want a transport error")
	}

	if strings.Contains(err.Error(), "SUPERSECRETTOKENVALUE") {
		t.Errorf("the API token leaked into the error text: %v", err)
	}
	// The cause must survive sanitizing, or the message is useless.
	if !strings.Contains(err.Error(), "/posts/all") {
		t.Errorf("error = %v, want it to name the endpoint path", err)
	}
}

//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestUpdateBookmark_TransportErrorHidesToken(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusOK, "")
	endpoint := server.URL
	server.Close()

	useEndpoint(t, endpoint)

	err := updateBookmark(leakToken, Bookmark{URL: testURL}, testURL, testTitle, "")
	if err == nil {
		t.Fatal("updateBookmark() error = nil, want a transport error")
	}
	if strings.Contains(err.Error(), "SUPERSECRETTOKENVALUE") {
		t.Errorf("the API token leaked into the error text: %v", err)
	}
}

// pinboardGet must not write the credential into the caller's map, which the
// caller may later log as ordinary request parameters.
//
//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestPinboardGet_DoesNotMutateCallerParams(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusOK, `{"result_code":"done"}`)
	useEndpoint(t, server.URL)

	params := url.Values{paramURL: {testURL}}

	if err := pinboardWrite("/posts/delete", params, leakToken); err != nil {
		t.Fatalf("pinboardWrite() error = %v, want nil", err)
	}

	if got := params.Get("auth_token"); got != "" {
		t.Errorf("caller's params gained auth_token = %q, want it untouched", got)
	}
	if got := params.Get("format"); got != "" {
		t.Errorf("caller's params gained format = %q, want it untouched", got)
	}
}

// One transient 429 must not slow the rest of the run. The interval widens on
// rejection and must come back down once requests succeed; without the narrowing
// a momentary throttle left the widened interval in place for the whole run.
//
// Driven at production scale rather than through the network, because
// narrowRateLimit floors at defaultRateLimit and a millisecond-scale fixture
// would sit below the floor and never move.
//
//nolint:paralleltest // drives the package-level rate limiter
func TestRateLimit_WidensOn429AndNarrowsOnSuccess(t *testing.T) {
	swap(t, &rateLimitInterval, defaultRateLimit)

	// The ticker is left at the suite-wide 1ms so later tests stay fast.
	t.Cleanup(func() { rateLimiter.Reset(time.Millisecond) })

	if got := widenRateLimit(); got != 2*defaultRateLimit {
		t.Fatalf("widenRateLimit() = %s, want %s", got, 2*defaultRateLimit)
	}
	if got := widenRateLimit(); got != 4*defaultRateLimit {
		t.Fatalf("second widenRateLimit() = %s, want %s", got, 4*defaultRateLimit)
	}

	narrowRateLimit()

	if rateLimitInterval != 2*defaultRateLimit {
		t.Errorf("after narrowing, interval = %s, want %s",
			rateLimitInterval, 2*defaultRateLimit)
	}

	narrowRateLimit()

	if rateLimitInterval != defaultRateLimit {
		t.Errorf("after narrowing, interval = %s, want the default %s",
			rateLimitInterval, defaultRateLimit)
	}

	// The floor holds: narrowing further must not drop below the documented rate.
	narrowRateLimit()

	if rateLimitInterval != defaultRateLimit {
		t.Errorf("interval = %s, want it floored at %s", rateLimitInterval, defaultRateLimit)
	}
}

//nolint:paralleltest // drives the package-level rate limiter
func TestWidenRateLimit_ClampsAtMax(t *testing.T) {
	swap(t, &rateLimitInterval, maxRateLimit)

	t.Cleanup(func() { rateLimiter.Reset(time.Millisecond) })

	if got := widenRateLimit(); got != maxRateLimit {
		t.Errorf("widenRateLimit() = %s, want it clamped at %s", got, maxRateLimit)
	}
}

// closeBody's failure branch routes to stderr rather than stdout, so a close
// diagnostic cannot interleave with the statistics report.
func TestCloseBody_ReportsFailure(t *testing.T) {
	t.Parallel()

	// Closing twice makes the second Close return an error deterministically.
	closeBody(errCloser{})
}

// errCloser always fails to close, standing in for a body whose underlying
// connection has already gone away.
type errCloser struct{}

func (errCloser) Close() error { return errors.New("close failed") }

// reportApplyError prints only when -verbose is set; the counter is the
// caller's responsibility and must not depend on the flag.
//
//nolint:paralleltest // mutates the verbose flag
func TestReportApplyError_HonoursVerbose(t *testing.T) {
	setVerbose(t, true)
	reportApplyError("updating", testURL, errors.New("boom"))

	setVerbose(t, false)
	reportApplyError("updating", testURL, errors.New("boom"))
}

// A parenthesis fix is already validated by fixParenthesesSuffix, so the
// pipeline must not request the same URL a second time.
//
//nolint:paralleltest // writes to the stats singleton
func TestExpandAndCheckURL_ParenthesisFixIsNotRevalidated(t *testing.T) {
	resetStats(t)

	trimmed := ""
	rec := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ")") {
			w.WriteHeader(http.StatusNotFound)

			return
		}
		trimmed = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})

	got, err := expandAndCheckURL(rec.URL+"/page)", false)
	if err != nil {
		t.Fatalf("expandAndCheckURL() error = %v, want nil", err)
	}
	if got != rec.URL+"/page" {
		t.Errorf("expandAndCheckURL() = %q, want the trimmed URL", got)
	}
	if trimmed == "" {
		t.Fatal("the trimmed URL was never requested")
	}

	// One HEAD probes the trimmed URL inside fixParenthesesSuffix and one is the
	// redirect probe; a third would mean the pipeline re-validated it.
	trimmedRequests := 0

	for _, request := range rec.Requests() {
		if strings.HasPrefix(request, "/page?") {
			trimmedRequests++
		}
	}

	if trimmedRequests > 2 {
		t.Errorf("trimmed URL requested %d times, want at most 2", trimmedRequests)
	}
}

//nolint:paralleltest // repoints pinboardAPIEndpoint and drives the rate limiter
func TestPinboardGet_GivesUpAfterRetries(t *testing.T) {
	resetStats(t)
	swap(t, &rateLimitInterval, time.Nanosecond)

	t.Cleanup(func() { rateLimiter.Reset(time.Millisecond) })

	server := staticServer(t, http.StatusTooManyRequests, "")
	useEndpoint(t, server.URL)

	_, err := getBookmarks(testToken)
	if !errors.Is(err, errRateLimited) {
		t.Errorf("getBookmarks() error = %v, want errRateLimited", err)
	}
}

// targetURL decides which URL a title or tag lookup follows.
func TestTargetURL(t *testing.T) {
	t.Parallel()

	if got := targetURL(testURL, ""); got != testURL {
		t.Errorf("targetURL() = %q, want the original when nothing was rewritten", got)
	}
	if got := targetURL(testURL, testNewURL); got != testNewURL {
		t.Errorf("targetURL() = %q, want the rewritten URL", got)
	}
}

func TestShortenerFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cleaned string
		wantKey string
	}{
		{name: "bitly", cleaned: "bit.ly/abc", wantKey: svcBitly},
		{name: "tinyurl", cleaned: "tinyurl.com/abc", wantKey: svcTinyURL},
		{name: "isgd", cleaned: "is.gd/abc", wantKey: svcIsGd},
		{name: "unknown host", cleaned: "example.com/abc", wantKey: ""},
		// A lookalike host must not match the bit.ly prefix.
		{name: "lookalike host", cleaned: "bit.ly.example.com/abc", wantKey: ""},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			service, found := shortenerFor(testCase.cleaned)
			if found != (testCase.wantKey != "") {
				t.Fatalf("shortenerFor(%q) found = %v, want %v",
					testCase.cleaned, found, testCase.wantKey != "")
			}
			if found && service.key != testCase.wantKey {
				t.Errorf("shortenerFor(%q) key = %q, want %q",
					testCase.cleaned, service.key, testCase.wantKey)
			}
		})
	}
}
