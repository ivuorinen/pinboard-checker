// safety_test.go covers the branches that stand between a bookmark and a
// deletion. Every one of them is the guard for a defect that previously
// destroyed data: an unreachable host, a shortener returning something that is
// not a web address, or a redirect pointing at a scheme net/http cannot dial.
package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// unreachableURL returns a URL guaranteed to fail at the transport layer: the
// server is started so the port is real, then closed before anyone dials it.
func unreachableURL(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	endpoint := server.URL
	server.Close()

	return endpoint
}

// A host that cannot be reached is not a host that said "gone". Deleting on a
// transport failure would destroy every bookmark behind a DNS outage.
//
//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestProcessBookmark_TransportFailureIsSkipped(t *testing.T) {
	resetStats(t)
	setSkipFlags(t, true, true)

	action := processBookmark(Bookmark{URL: unreachableURL(t), Title: "t"}, testToken)

	if action.Action != ActionNoChange {
		t.Errorf("processBookmark() action = %v, want ActionNoChange", action.Action)
	}
	if !errors.Is(action.Error, errUnverifiable) {
		t.Errorf("action.Error = %v, want errUnverifiable", action.Error)
	}
	if stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", stats.Skipped)
	}
	if stats.ActionDelete != 0 {
		t.Errorf("ActionDelete = %d, want 0 for an unreachable host", stats.ActionDelete)
	}
}

// A shortener that resolves to a non-web scheme must not have that value
// written back as the bookmark's URL.
//
//nolint:paralleltest // mutates the shortener seam and the stats singleton
func TestExpandAndCheckURL_RejectsNonHTTPExpansion(t *testing.T) {
	resetStats(t)

	swap(t, &unshortenIsGd, func(_ string) (string, error) {
		return testFTPURL, nil
	})

	_, err := expandAndCheckURL("https://is.gd/abc", false)
	if !errors.Is(err, errUnverifiable) {
		t.Errorf("expandAndCheckURL() error = %v, want errUnverifiable", err)
	}
	if errors.Is(err, errDeadLink) {
		t.Error("an unexpandable scheme must not be reported as a dead link")
	}
}

// Likewise for a redirect: a Location pointing at mailto: or ftp: is not a
// destination this tool can verify, and must not become the bookmark.
//
//nolint:paralleltest // writes to the stats singleton
func TestFollowRedirects_RejectsNonHTTPRedirect(t *testing.T) {
	resetStats(t)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "mailto:someone@example.com")
			w.WriteHeader(http.StatusMovedPermanently)
		}))
	t.Cleanup(server.Close)

	_, err := followRedirects(server.URL, false)
	if !errors.Is(err, errUnverifiable) {
		t.Errorf("followRedirects() error = %v, want errUnverifiable", err)
	}
}

// When a host refuses HEAD and the GET retry also fails, the HEAD status stands
// rather than the failure being swallowed into a success.
//
//nolint:paralleltest // writes to the stats singleton
func TestConfirmWithGet_KeepsHeadStatusWhenGetFails(t *testing.T) {
	resetStats(t)

	if got := confirmWithGet(unreachableURL(t), http.StatusForbidden); got != http.StatusForbidden {
		t.Errorf("confirmWithGet() = %d, want the original HEAD status", got)
	}
}

// A 403 the GET retry also gets is still not a dead link. The GET retry exists
// for bot protection, but it sends Go's default User-Agent and no JS, so it is
// answered 403 by exactly the protection it was written to see past — which
// made a confirmed 403 a deletion of a live page.
//
//nolint:paralleltest // writes to the stats singleton
func TestCheckURL_ConfirmedForbiddenIsUnverifiable(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusForbidden, "")

	_, err := checkURL(server.URL, "test context")
	if !errors.Is(err, errUnverifiable) {
		t.Errorf("checkURL() error = %v, want errUnverifiable when GET also refuses", err)
	}
	if errors.Is(err, errDeadLink) {
		t.Error("a refused request must not be reported as a dead link")
	}
}

// The status set that may delete a bookmark, pinned. Only an answer about the
// resource counts; every other 4xx describes the request and must be skipped.
//
//nolint:paralleltest // writes to the stats singleton
func TestCheckURL_OnlyGoneStatusesDelete(t *testing.T) {
	tests := []struct {
		status int
		dead   bool
	}{
		{http.StatusNotFound, true},
		{http.StatusGone, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusMethodNotAllowed, false},
		{http.StatusRequestTimeout, false},
		{http.StatusTooManyRequests, false},
		{http.StatusUnavailableForLegalReasons, false},
	}

	for _, testCase := range tests {
		t.Run(http.StatusText(testCase.status), func(t *testing.T) {
			resetStats(t)

			server := staticServer(t, testCase.status, "")

			_, err := checkURL(server.URL, "test context")
			if got := errors.Is(err, errDeadLink); got != testCase.dead {
				t.Errorf("status %d: errDeadLink = %v, want %v (err = %v)",
					testCase.status, got, testCase.dead, err)
			}
			if !testCase.dead && !errors.Is(err, errUnverifiable) {
				t.Errorf("status %d: error = %v, want errUnverifiable",
					testCase.status, err)
			}
		})
	}
}

// If the trimmed variant cannot be reached at all, the original URL stands;
// reporting an error here would route the bookmark to deletion.
//
//nolint:paralleltest // writes to the stats singleton
func TestFixParenthesesSuffix_UnreachableTrimmedURLKeepsOriginal(t *testing.T) {
	resetStats(t)

	original := unreachableURL(t) + "/page)"

	got, err := fixParenthesesSuffix(original, false)
	if err != nil {
		t.Fatalf("fixParenthesesSuffix() error = %v, want nil", err)
	}
	if got != original {
		t.Errorf("fixParenthesesSuffix() = %q, want the original %q", got, original)
	}
	if stats.ParenthesesFixed != 0 {
		t.Errorf("ParenthesesFixed = %d, want 0", stats.ParenthesesFixed)
	}
}

// A failed delete of the stale URL leaves a duplicate behind, so it must be
// counted even though the add succeeded.
//
//nolint:paralleltest // mutates the stats singleton and the verbose flag
func TestApplyUpdateWrite_FailedOldURLDeleteIsCounted(t *testing.T) {
	resetStats(t)
	setVerbose(t, false)

	server := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)

		if strings.HasPrefix(r.URL.Path, "/posts/delete") {
			writeBody(t, w, `{"result_code":"item not found"}`)

			return
		}

		writeBody(t, w, `{"result_code":"done"}`)
	})
	useEndpoint(t, server.URL)

	applyUpdateWrite(BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
	}, testToken)

	// The add landed, so the update counts; the delete did not, so the failure
	// counts too. Reporting only one of the two would misstate the outcome.
	if stats.UpdatesPerformed != 1 {
		t.Errorf("UpdatesPerformed = %d, want 1", stats.UpdatesPerformed)
	}
	if stats.ApplyErrors != 1 {
		t.Errorf("ApplyErrors = %d, want 1 for the failed delete", stats.ApplyErrors)
	}
}

// A redirect target is chosen by a remote party, so following one into private
// address space let a bookmarked host steer the tool at localhost or the cloud
// metadata endpoint — whose page title would then be written to Pinboard.
//
// No network: every case resolves as a literal IP.
func TestIsPublicHostImpl(t *testing.T) {
	t.Parallel()

	tests := []struct {
		rawURL string
		public bool
	}{
		{"http://127.0.0.1:8080/admin", false},
		{"http://169.254.169.254/latest/meta-data/", false},
		{"http://10.0.0.5/", false},
		{"http://192.168.1.1/", false},
		{"http://172.16.0.1/", false},
		{"http://[::1]/", false},
		{"http://0.0.0.0/", false},
		{"http://8.8.8.8/", true},
		{"http://93.184.216.34/", true},
		// Unparseable, and a URL with no host at all: neither can be verified,
		// so neither may be followed.
		{testBadURL, false},
		{"file:///etc/passwd", false},
	}

	for _, testCase := range tests {
		t.Run(testCase.rawURL, func(t *testing.T) {
			t.Parallel()

			if got := isPublicHostImpl(testCase.rawURL); got != testCase.public {
				t.Errorf("isPublicHostImpl(%q) = %v, want %v",
					testCase.rawURL, got, testCase.public)
			}
		})
	}
}

// A redirect into private space must be skipped, never deleted: the tool could
// not check the real destination, so it has no verdict to act on.
//
//nolint:paralleltest // mutates the package-level guard and the stats singleton
func TestFollowRedirects_RejectsPrivateRedirectTarget(t *testing.T) {
	resetStats(t)
	swap(t, &isPublicHost, isPublicHostImpl)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
			w.WriteHeader(http.StatusFound)
		}))
	t.Cleanup(server.Close)

	_, err := followRedirects(server.URL, false)
	if !errors.Is(err, errUnverifiable) {
		t.Errorf("followRedirects() error = %v, want errUnverifiable", err)
	}
	if errors.Is(err, errDeadLink) {
		t.Error("a redirect into private space must not be reported as a dead link")
	}
}

// A shortener answering with a private address must be refused as firmly as a
// redirect to one: the expansion is written straight back as the bookmark URL.
//
//nolint:paralleltest // mutates the package-level guard
func TestValidExpandedURL_RejectsNonPublicHost(t *testing.T) {
	swap(t, &isPublicHost, isPublicHostImpl)

	if _, err := validExpandedURL("http://127.0.0.1:8080/admin"); err == nil {
		t.Error("validExpandedURL() error = nil, want a rejection for a loopback host")
	}

	// The guard must not reject an ordinary public destination.
	if _, err := validExpandedURL("http://8.8.8.8/page"); err != nil {
		t.Errorf("validExpandedURL() error = %v, want nil for a public host", err)
	}
}

// A repair step that fails must abort the pipeline rather than carrying a
// half-repaired URL into the liveness check.
//
//nolint:paralleltest // mutates the repair pipeline and the stats singleton
func TestExpandAndCheckURL_StepFailureIsReturned(t *testing.T) {
	resetStats(t)

	boom := errors.New("step exploded")

	// A step that fails: neither real step does today, so without this the
	// pipeline's error path would never run.
	swap(t, &repairSteps, []urlStep{
		{name: "failing step", fn: func(string, bool) (string, error) { return "", boom }},
	})

	_, err := expandAndCheckURL(testURL, false)
	if !errors.Is(err, boom) {
		t.Errorf("expandAndCheckURL() error = %v, want the step's error", err)
	}
}

// The dispatch table's tinyurl entry must actually route to the tinyurl
// expander; a table whose closures are never exercised is a table that can be
// mis-wired without any test failing.
//
//nolint:paralleltest // mutates the shortener seam and the stats singleton
func TestExpandURL_DispatchesTinyURL(t *testing.T) {
	resetStats(t)

	swap(t, &unshortenTinyURL, func(_ string) (string, error) {
		return testFinalURL, nil
	})

	got, err := expandURL("https://tinyurl.com/abc", false)
	if err != nil {
		t.Fatalf("expandURL() error = %v, want nil", err)
	}
	if got != testFinalURL {
		t.Errorf("expandURL() = %q, want %q", got, testFinalURL)
	}
	if stats.ShortURLsExpanded[svcTinyURL] != 1 {
		t.Errorf("expected one recorded tinyurl expansion, got %d",
			stats.ShortURLsExpanded[svcTinyURL])
	}
}

// A caller that already chose a User-Agent keeps it; the transport fills one in
// only where none was set.
func TestUserAgentTransport_KeepsCallerUserAgent(t *testing.T) {
	t.Parallel()

	var seen string

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = r.UserAgent()
			w.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(server.Close)

	client := &http.Client{Transport: newUserAgentTransport()}

	request, err := http.NewRequestWithContext(t.Context(),
		http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	request.Header.Set("User-Agent", "caller-chosen/1.0")

	//nolint:bodyclose // closeBody below closes it; bodyclose cannot see through the helper
	resp, err := client.Do(request)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer closeBody(resp.Body)

	if seen != "caller-chosen/1.0" {
		t.Errorf("User-Agent = %q, want the caller's own", seen)
	}
}

// And where none was set, the tool names itself.
func TestUserAgentTransport_SetsDefaultUserAgent(t *testing.T) {
	t.Parallel()

	var seen string

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = r.UserAgent()
			w.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(server.Close)

	client := &http.Client{Transport: newUserAgentTransport()}

	//nolint:bodyclose // closeBody below closes it; bodyclose cannot see through the helper
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer closeBody(resp.Body)

	if !strings.HasPrefix(seen, "pinboard-checker/") {
		t.Errorf("User-Agent = %q, want it to name the tool", seen)
	}
	if !strings.Contains(seen, "github.com/ivuorinen/pinboard-checker") {
		t.Errorf("User-Agent = %q, want it to name the repository", seen)
	}
}

// Requests to one host must be spaced, or a worker pool checking many
// bookmarks on one domain bursts it and is answered 429 — which this tool then
// has to interpret, having caused it itself.
//
//nolint:paralleltest // mutates the package-level host throttle
func TestWaitForHost_SpacesRequestsToOneHost(t *testing.T) {
	const interval = 20 * time.Millisecond

	swap(t, &perHostInterval, interval)

	previousSlots := hostNextSlot
	t.Cleanup(func() { hostNextSlot = previousSlots })
	hostNextSlot = map[string]time.Time{}

	server := staticServer(t, http.StatusOK, "")

	start := time.Now()
	for range 3 {
		waitForHost(server.URL)
	}
	elapsed := time.Since(start)

	// Three reservations: the first is immediate, the next two each wait a full
	// interval, so at least two intervals must have passed.
	if elapsed < 2*interval {
		t.Errorf("three requests to one host took %s, want at least %s",
			elapsed, 2*interval)
	}
}

// Different hosts must not throttle each other, or the pool serializes on a
// budget that was never meant to be global.
//
//nolint:paralleltest // mutates the package-level host throttle
func TestWaitForHost_DoesNotSpaceDistinctHosts(t *testing.T) {
	swap(t, &perHostInterval, time.Hour)

	previousSlots := hostNextSlot
	t.Cleanup(func() { hostNextSlot = previousSlots })
	hostNextSlot = map[string]time.Time{}

	first := staticServer(t, http.StatusOK, "")
	second := staticServer(t, http.StatusOK, "")

	start := time.Now()
	waitForHost(first.URL)
	waitForHost(second.URL)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("two distinct hosts took %s; they must not share a slot", elapsed)
	}
}

// setKnownURLs points the run's existing-URL set at a fixed list and restores
// it afterwards, the same save-restore shape the other package-level seams use.
func setKnownURLs(t *testing.T, urls ...string) {
	t.Helper()

	previous := knownURLs
	t.Cleanup(func() { knownURLs = previous })

	knownURLs = make(map[string]bool, len(urls))
	for _, u := range urls {
		knownURLs[u] = true
	}
}

// A URL that resolves onto a bookmark the account already holds must not be
// written: posts/add with replace=yes overwrites the destination entirely, so
// the existing bookmark's notes and tags would be replaced by the mover's.
// Only the duplicate is removed.
//
//nolint:paralleltest // mutates the stats singleton, the verbose flag, and knownURLs
func TestApplyUpdateWrite_ExistingTargetIsMergedNotOverwritten(t *testing.T) {
	resetStats(t)
	setKnownURLs(t, testURL, testNewURL)

	server := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, `{"result_code":"done"}`)
		_ = r
	})
	useEndpoint(t, server.URL)

	// Verbose so the merge's own report line runs; a silent merge would leave
	// the user unable to tell a collapsed duplicate from a plain update.
	setVerbose(t, true)

	printed := captureOutput(func() {
		applyUpdateWrite(BookmarkAction{
			Original: Bookmark{URL: testURL, Title: "mover", Tags: "mover-tag"},
			Action:   ActionUpdate,
			NewURL:   testNewURL,
		}, testToken)
	})

	if !strings.Contains(printed, "Merged:") {
		t.Errorf("verbose output = %q, want it to report the merge", printed)
	}

	for _, request := range server.Requests() {
		if strings.HasPrefix(request, "/posts/add") {
			t.Errorf("posts/add was called for a URL the account already holds: %s", request)
		}
	}

	if stats.Merged != 1 {
		t.Errorf("Merged = %d, want 1", stats.Merged)
	}
	if stats.UpdatesPerformed != 0 {
		t.Errorf("UpdatesPerformed = %d, want 0; nothing was written", stats.UpdatesPerformed)
	}
	if stats.ApplyErrors != 0 {
		t.Errorf("ApplyErrors = %d, want 0", stats.ApplyErrors)
	}
}

// A merge whose delete Pinboard refuses leaves the duplicate in place, so it
// must be counted as an apply error — that count is what makes the run exit
// non-zero, and a silently failed merge would report a clean run.
//
//nolint:paralleltest // mutates the stats singleton, the verbose flag, and knownURLs
func TestApplyUpdateWrite_FailedMergeDeleteIsCounted(t *testing.T) {
	resetStats(t)
	setVerbose(t, false)
	setKnownURLs(t, testURL, testNewURL)

	server := staticServer(t, http.StatusOK, `{"result_code":"item not found"}`)
	useEndpoint(t, server.URL)

	applyUpdateWrite(BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
	}, testToken)

	if stats.ApplyErrors != 1 {
		t.Errorf("ApplyErrors = %d, want 1 for the failed merge delete", stats.ApplyErrors)
	}
	if stats.Merged != 0 {
		t.Errorf("Merged = %d, want 0; the duplicate is still there", stats.Merged)
	}
}

// A destination the account does not hold is still written normally; the merge
// path must not swallow ordinary URL rewrites.
//
//nolint:paralleltest // mutates the stats singleton, the verbose flag, and knownURLs
func TestApplyUpdateWrite_NewTargetIsWritten(t *testing.T) {
	resetStats(t)
	setVerbose(t, false)
	setKnownURLs(t, testURL)

	server := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, `{"result_code":"done"}`)
		_ = r
	})
	useEndpoint(t, server.URL)

	applyUpdateWrite(BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
	}, testToken)

	if stats.UpdatesPerformed != 1 {
		t.Errorf("UpdatesPerformed = %d, want 1", stats.UpdatesPerformed)
	}
	if stats.Merged != 0 {
		t.Errorf("Merged = %d, want 0", stats.Merged)
	}
}

// Pinboard answers some endpoints with "result" rather than "result_code".
//
//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestDecodePinboardResult_AcceptsResultField(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusOK, `{"result":"done"}`)
	useEndpoint(t, server.URL)

	if err := deleteBookmark(testToken, testURL); err != nil {
		t.Errorf("deleteBookmark() error = %v, want nil for a `result` document", err)
	}
}

// A body that is not the expected document must fail rather than be read as a
// success: silently accepting garbage is how a rejected write looked applied.
//
//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestPinboardResponses_RejectMalformedJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
		call func() error
	}{
		{
			name: "write result",
			body: "not json at all",
			call: func() error { return deleteBookmark(testToken, testURL) },
		},
		{
			name: "bookmark list",
			body: `{"not":"an array"}`,
			call: func() error {
				_, err := getBookmarks(testToken)

				return err
			},
		},
		{
			name: "tag suggestions",
			body: "<suggested><popular>go</popular></suggested>",
			call: func() error {
				_, err := getSuggestedTags(testToken, testURL)

				return err
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resetStats(t)

			server := staticServer(t, http.StatusOK, testCase.body)
			useEndpoint(t, server.URL)

			if err := testCase.call(); err == nil {
				t.Error("error = nil, want a decode failure")
			}
		})
	}
}

// Every shortener must report a transport failure rather than return an empty
// URL that would later be judged a dead link.
//
//nolint:paralleltest // repoints the shortener endpoints
func TestShorteners_ReportTransportFailures(t *testing.T) {
	t.Run("is.gd", func(t *testing.T) {
		useIsGdEndpoint(t, unreachableURL(t))

		if _, err := unshortenIsGdImpl("https://is.gd/abc"); err == nil {
			t.Error("unshortenIsGdImpl() error = nil, want a transport error")
		}
	})

	t.Run("tinyurl", func(t *testing.T) {
		if _, err := unshortenTinyURLImpl(unreachableURL(t)); err == nil {
			t.Error("unshortenTinyURLImpl() error = nil, want a transport error")
		}
	})

	t.Run("bitly", func(t *testing.T) {
		t.Setenv(bitlyTokenName, "bitly-token")
		useBitlyEndpoint(t, unreachableURL(t))

		if _, err := unshortenBitlyImpl(testBitlyURL); err == nil {
			t.Error("unshortenBitlyImpl() error = nil, want a transport error")
		}
	})
}

// A shortener answering with a malformed document must fail rather than yield
// an empty URL.
//
//nolint:paralleltest // repoints the shortener endpoints
func TestShorteners_RejectMalformedJSON(t *testing.T) {
	t.Run("is.gd", func(t *testing.T) {
		server := staticServer(t, http.StatusOK, "not json")
		useIsGdEndpoint(t, server.URL)

		if _, err := unshortenIsGdImpl("https://is.gd/abc"); err == nil {
			t.Error("unshortenIsGdImpl() error = nil, want a decode failure")
		}
	})

	t.Run("bitly", func(t *testing.T) {
		t.Setenv(bitlyTokenName, "bitly-token")

		server := staticServer(t, http.StatusOK, "not json")
		useBitlyEndpoint(t, server.URL)

		if _, err := unshortenBitlyImpl(testBitlyURL); err == nil {
			t.Error("unshortenBitlyImpl() error = nil, want a decode failure")
		}
	})
}

// A failed lookup must yield no value and be counted as a failure. Returning a
// partial or garbage title would write it to the bookmark.
//
//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestFetchIfNeeded_FailureYieldsNothingAndCounts(t *testing.T) {
	t.Run("title", func(t *testing.T) {
		resetStats(t)
		setSkipFlags(t, false, true)

		server := staticServer(t, http.StatusNotFound, "")

		if got := fetchTitleIfNeeded(Bookmark{URL: server.URL}, ""); got != "" {
			t.Errorf("fetchTitleIfNeeded() = %q, want empty on failure", got)
		}
		if stats.TitlesFailed != 1 {
			t.Errorf("TitlesFailed = %d, want 1", stats.TitlesFailed)
		}
		if stats.TitlesFetched != 0 {
			t.Errorf("TitlesFetched = %d, want 0", stats.TitlesFetched)
		}
	})

	t.Run("tags", func(t *testing.T) {
		resetStats(t)
		setSkipFlags(t, true, false)

		server := staticServer(t, http.StatusNotFound, "")
		useEndpoint(t, server.URL)

		if got := fetchTagsIfNeeded(Bookmark{URL: testURL}, testToken, ""); got != "" {
			t.Errorf("fetchTagsIfNeeded() = %q, want empty on failure", got)
		}
		if stats.TagsFailed != 1 {
			t.Errorf("TagsFailed = %d, want 1", stats.TagsFailed)
		}
		if stats.TagsFetched != 0 {
			t.Errorf("TagsFetched = %d, want 0", stats.TagsFetched)
		}
	})
}

// A call that succeeded and had nothing to take is not a failure. Both are
// ordinary: most pages Pinboard has no tag suggestions for, and a page may
// carry no <title> at all. Counting them as failures made a run where every
// response was 200 report hundreds of them.
//
//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestFetchIfNeeded_EmptyResultIsNotAFailure(t *testing.T) {
	t.Run("tags with no suggestions", func(t *testing.T) {
		resetStats(t)
		setSkipFlags(t, true, false)

		server := staticServer(t, http.StatusOK, `[]`)
		useEndpoint(t, server.URL)

		if got := fetchTagsIfNeeded(Bookmark{URL: testURL}, testToken, ""); got != "" {
			t.Errorf("fetchTagsIfNeeded() = %q, want empty when nothing is suggested", got)
		}
		if stats.TagsEmpty != 1 {
			t.Errorf("TagsEmpty = %d, want 1", stats.TagsEmpty)
		}
		if stats.TagsFailed != 0 {
			t.Errorf("TagsFailed = %d, want 0; the call succeeded", stats.TagsFailed)
		}
	})

	t.Run("title on a page without one", func(t *testing.T) {
		resetStats(t)
		setSkipFlags(t, false, true)

		server := staticServer(t, http.StatusOK, "<html><body>no title here</body></html>")

		if got := fetchTitleIfNeeded(Bookmark{URL: server.URL}, ""); got != "" {
			t.Errorf("fetchTitleIfNeeded() = %q, want empty when the page has no title", got)
		}
		if stats.TitlesEmpty != 1 {
			t.Errorf("TitlesEmpty = %d, want 1", stats.TitlesEmpty)
		}
		if stats.TitlesFailed != 0 {
			t.Errorf("TitlesFailed = %d, want 0; the fetch succeeded", stats.TitlesFailed)
		}
	})
}

// A connection that dies mid-body must fail rather than yield whatever was
// parsed from the fragment received so far, which would be written to the
// bookmark as its title.
//
//nolint:paralleltest // writes to the stats singleton
func TestGetPageTitle_TruncatedResponse(t *testing.T) {
	resetStats(t)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusOK)
			writeBody(t, w, "<html><head><title>Partial")

			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}

			// Aborts the response without completing Content-Length, so the
			// client's next Read fails.
			panic(http.ErrAbortHandler)
		}))
	t.Cleanup(server.Close)

	// The handler panic is deliberate; keep it out of the test log.
	server.Config.ErrorLog = discardLogger()

	if _, err := getPageTitle(server.URL); err == nil {
		t.Error("getPageTitle() error = nil, want a failure on a truncated body")
	}
}

// discardLogger silences the httptest server's own logging for tests that
// deliberately abort a response.
func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

//nolint:paralleltest // writes to the stats singleton
func TestGetPageTitle_TransportFailure(t *testing.T) {
	resetStats(t)

	if _, err := getPageTitle(unreachableURL(t)); err == nil {
		t.Error("getPageTitle() error = nil, want a transport error")
	}
}

// getPageTitle must reject a body that is not parseable as a document rather
// than write whatever it extracted into the bookmark title.
//
//nolint:paralleltest // writes to the stats singleton
func TestGetPageTitle_UnparseableContentType(t *testing.T) {
	resetStats(t)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			// A media type with an illegal parameter cannot be parsed.
			w.Header().Set("Content-Type", "text/html; charset")
			w.WriteHeader(http.StatusOK)
			writeBody(t, w, "<html><head><title>x</title></head></html>")
		}))
	t.Cleanup(server.Close)

	if _, err := getPageTitle(server.URL); err == nil {
		t.Error("getPageTitle() error = nil, want an unparseable Content-Type error")
	}
}

// updateBookmark prefers freshly fetched tags over the bookmark's existing set.
//
//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestUpdateBookmark_PrefersNewTags(t *testing.T) {
	resetStats(t)

	rec := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, `{"result_code":"done"}`)
	})
	useEndpoint(t, rec.URL)

	bookmark := Bookmark{URL: testURL, Tags: "old"}
	if err := updateBookmark(testToken, bookmark, testURL, "", "fresh,tags"); err != nil {
		t.Fatalf("updateBookmark() error = %v, want nil", err)
	}

	request := rec.Requests()[0]
	if !strings.Contains(request, "tags=fresh%2Ctags") {
		t.Errorf("request %q, want the new tags", request)
	}
	if strings.Contains(request, "tags=old") {
		t.Errorf("request %q, want the old tags replaced", request)
	}
}
