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

// newRunChecker returns a test checker for a whole pass against a Pinboard
// fixture: -ci and both lookups off, so a test asserts the run's contract
// rather than the bookmark processing it contains.
func newRunChecker(t *testing.T, endpoint string) *checker {
	t.Helper()

	chk := newTestChecker(t)
	chk.pinboardEndpoint = endpoint
	chk.ciMode = true
	chk.skipTitles = true
	chk.skipAutoTags = true
	chk.workers = 1

	return chk
}

func TestRun_CleanPassExitsZero(t *testing.T) {
	t.Parallel()

	if code := newRunChecker(t, staticServer(t, http.StatusOK, `[]`).URL).run(); code != 0 {
		t.Errorf("run() = %d, want 0 for a clean pass", code)
	}
}

// -ci drops the report but not the outcome. A -ci run that deleted bookmarks
// and failed nothing used to print nothing at all; the count line is printed on
// every -ci run, clean ones included.
func TestRun_CIModePrintsCountLine(t *testing.T) {
	t.Parallel()

	chk := newRunChecker(t, staticServer(t, http.StatusOK, `[]`).URL)

	if code := chk.run(); code != 0 {
		t.Errorf("run() = %d, want 0", code)
	}

	want := "updated 0, deleted 0, merged 0, skipped 0, rejected 0"
	if output := stderrOf(chk); !strings.Contains(output, want) {
		t.Errorf("stderr = %q, want the -ci count line %q", output, want)
	}
}

// Returning from main exited 0, which let a scheduled run report success after
// fetching nothing.
func TestRun_FetchFailureExitsNonZero(t *testing.T) {
	t.Parallel()

	chk := newRunChecker(t,
		staticServer(t, http.StatusUnauthorized, "API requires authentication").URL)

	if code := chk.run(); code != 1 {
		t.Errorf("run() = %d, want 1 when the fetch fails", code)
	}
}

// A run that applies nothing because every write was rejected must not look
// clean: the counters are what distinguish it from a no-op pass.
func TestRun_RejectedWriteExitsNonZero(t *testing.T) {
	t.Parallel()

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

	chk := newRunChecker(t, server.URL)
	chk.skipTitles = false

	code := chk.run()

	if chk.stats.ApplyErrorCount() != 1 {
		t.Fatalf("ApplyErrorCount() = %d, want 1", chk.stats.ApplyErrorCount())
	}
	if code != 1 {
		t.Errorf("run() = %d, want 1 when a write is rejected", code)
	}
}

// Without -ci the run prints its statistics block, which is the only report the
// tool produces.
func TestRun_PrintsStatisticsWhenNotCIMode(t *testing.T) {
	t.Parallel()

	chk := newRunChecker(t, staticServer(t, http.StatusOK, `[]`).URL)
	chk.ciMode = false

	if code := chk.run(); code != 0 {
		t.Errorf("run() = %d, want 0", code)
	}

	if output := stdoutOf(chk); !strings.Contains(output, "PROCESSING STATISTICS") {
		t.Errorf("output = %q, want the statistics block", output)
	}
}

func TestRun_DryRunIssuesNoWrites(t *testing.T) {
	t.Parallel()

	target := staticServer(t, http.StatusNotFound, "")

	server := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)

		if strings.HasPrefix(r.URL.Path, "/posts/all") {
			writeBody(t, w, `[{"href":"`+target.URL+`","description":"t","tags":"x"}]`)

			return
		}

		writeBody(t, w, testDoneBody)
	})

	chk := newRunChecker(t, server.URL)
	chk.dryRun = true

	if code := chk.run(); code != 0 {
		t.Errorf("run() = %d, want 0 for a dry run", code)
	}

	// The dead bookmark is scheduled for deletion but nothing may be sent.
	for _, request := range server.Requests() {
		if strings.HasPrefix(request, "/posts/delete") || strings.HasPrefix(request, "/posts/add") {
			t.Errorf("dry run issued a write: %s", request)
		}
	}
}

// Both clients must be rebuilt. Dropping the second is an easy refactor slip
// and would leave the redirect probe unbounded.
func TestSetHTTPTimeout(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.setHTTPTimeout(7 * time.Second)

	if chk.client.Timeout != 7*time.Second {
		t.Errorf("client.Timeout = %s, want 7s", chk.client.Timeout)
	}
	if chk.redirectClient.Timeout != 7*time.Second {
		t.Errorf("redirectClient.Timeout = %s, want 7s", chk.redirectClient.Timeout)
	}
}

// closedEndpoint returns the URL of a server that is already closed, which
// gives a guaranteed dial failure.
func closedEndpoint(t *testing.T) string {
	t.Helper()

	server := staticServer(t, http.StatusOK, "")
	endpoint := server.URL
	server.Close()

	return endpoint
}

// The auth token travels in the query string, and url.Error renders the full
// URL, so wrapping a transport failure printed the credential to stderr.
func TestPinboardGet_TransportErrorHidesToken(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.apiToken = leakToken
	chk.pinboardEndpoint = closedEndpoint(t)

	_, err := chk.getBookmarks()
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

func TestUpdateBookmark_TransportErrorHidesToken(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.apiToken = leakToken
	chk.pinboardEndpoint = closedEndpoint(t)

	err := chk.updateBookmark(Bookmark{URL: testURL}, testURL, testTitle, "")
	if err == nil {
		t.Fatal("updateBookmark() error = nil, want a transport error")
	}
	if strings.Contains(err.Error(), "SUPERSECRETTOKENVALUE") {
		t.Errorf("the API token leaked into the error text: %v", err)
	}
}

// pinboardGet must not write the credential into the caller's map, which the
// caller may later log as ordinary request parameters.
func TestPinboardGet_DoesNotMutateCallerParams(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.apiToken = leakToken
	chk.pinboardEndpoint = staticServer(t, http.StatusOK, testDoneBody).URL

	params := url.Values{paramURL: {testURL}}

	if err := chk.pinboardWrite("/posts/delete", params); err != nil {
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
func TestRateLimit_WidensOn429AndNarrowsOnSuccess(t *testing.T) {
	t.Parallel()

	limiter := newRateLimiter(defaultRateLimit)

	if got := limiter.widen(); got != 2*defaultRateLimit {
		t.Fatalf("widen() = %s, want %s", got, 2*defaultRateLimit)
	}
	if got := limiter.widen(); got != 4*defaultRateLimit {
		t.Fatalf("second widen() = %s, want %s", got, 4*defaultRateLimit)
	}

	limiter.narrow()

	if got := limiter.interval; got != 2*defaultRateLimit {
		t.Errorf("after narrowing, interval = %s, want %s", got, 2*defaultRateLimit)
	}

	limiter.narrow()

	if got := limiter.interval; got != defaultRateLimit {
		t.Errorf("after narrowing, interval = %s, want the default %s", got, defaultRateLimit)
	}

	// The floor holds: narrowing further must not drop below the documented rate.
	limiter.narrow()

	if got := limiter.interval; got != defaultRateLimit {
		t.Errorf("interval = %s, want it floored at %s", got, defaultRateLimit)
	}
}

func TestRateLimiter_WidenClampsAtMax(t *testing.T) {
	t.Parallel()

	if got := newRateLimiter(maxRateLimit).widen(); got != maxRateLimit {
		t.Errorf("widen() = %s, want it clamped at %s", got, maxRateLimit)
	}
}

// The first call of a run goes at once. The ticker the limiter used to be
// delivered its first tick a whole interval after construction, so every run
// waited 3 s before fetching a single bookmark.
func TestRateLimiter_FirstCallDoesNotWait(t *testing.T) {
	t.Parallel()

	limiter := newRateLimiter(time.Hour)

	done := make(chan struct{})
	go func() {
		limiter.wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("first wait() blocked, want it to return at once")
	}
}

// Every call after the first starts a full interval after the one before it,
// and a widened interval holds the retry back by the new value. Each clock
// starts before the slot is booked, so the bound holds to the nanosecond.
func TestRateLimiter_SpacesCallsByTheInterval(t *testing.T) {
	t.Parallel()

	const interval = 50 * time.Millisecond

	limiter := newRateLimiter(interval)

	start := time.Now()
	limiter.wait()
	limiter.wait()

	if elapsed := time.Since(start); elapsed < interval {
		t.Errorf("two wait() calls took %s, want at least %s", elapsed, interval)
	}

	start = time.Now()
	widened := limiter.widen()
	limiter.wait()

	if elapsed := time.Since(start); elapsed < widened {
		t.Errorf("wait() after widen returned after %s, want at least %s", elapsed, widened)
	}
}

// closeBody's failure branch routes to the diagnostic stream rather than the
// report stream, so a close diagnostic cannot interleave with the statistics.
func TestCloseBody_ReportsFailure(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.closeBody(errCloser{})

	if output := stderrOf(chk); !strings.Contains(output, "close failed") {
		t.Errorf("stderr = %q, want the close failure", output)
	}
	if output := stdoutOf(chk); output != "" {
		t.Errorf("stdout = %q, want nothing on the report stream", output)
	}
}

// errCloser always fails to close, standing in for a body whose underlying
// connection has already gone away.
type errCloser struct{}

func (errCloser) Close() error { return errors.New("close failed") }

// reportApplyError prints only when -verbose is set; the counter is the
// caller's responsibility and must not depend on the flag.
func TestReportApplyError_HonoursVerbose(t *testing.T) {
	t.Parallel()

	for _, verbose := range []bool{true, false} {
		chk := newTestChecker(t)
		chk.verbose = verbose
		chk.reportApplyError("updating", testURL, errors.New("boom"))

		output := stderrOf(chk)
		if printed := strings.Contains(output, "Error updating "+testURL); printed != verbose {
			t.Errorf("verbose %v: stderr = %q, want the error printed only under -verbose",
				verbose, output)
		}
	}
}

// A parenthesis fix is already validated by fixParenthesesSuffix, so the
// pipeline must not request the same URL a second time.
func TestExpandAndCheckURL_ParenthesisFixIsNotRevalidated(t *testing.T) {
	t.Parallel()

	trimmed := ""
	rec := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ")") {
			w.WriteHeader(http.StatusNotFound)

			return
		}
		trimmed = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})

	got, err := newTestChecker(t).expandAndCheckURL(rec.URL + "/page)")
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

func TestPinboardGet_GivesUpAfterRetries(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.pinboardEndpoint = staticServer(t, http.StatusTooManyRequests, "").URL

	if _, err := chk.getBookmarks(); !errors.Is(err, errRateLimited) {
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
