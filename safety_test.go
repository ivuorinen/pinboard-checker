// safety_test.go covers the branches that stand between a bookmark and a
// deletion. Every one of them is the guard for a defect that previously
// destroyed data: an unreachable host, a redirect pointing at a scheme
// net/http cannot dial, or a status that describes access rather than
// existence.
package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// closeResponse closes a response body inside a test and fails the test if the
// close fails.
func closeResponse(t *testing.T, resp *http.Response) {
	t.Helper()

	if err := resp.Body.Close(); err != nil {
		t.Errorf("failed to close response body: %v", err)
	}
}

// A host that cannot be reached is not a host that said "gone". Deleting on a
// transport failure would destroy every bookmark behind a DNS outage.
func TestProcessBookmark_TransportFailureIsSkipped(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.skipTitles = true
	chk.skipAutoTags = true

	action := chk.processBookmark(Bookmark{URL: unreachableURL(t), Title: "t"})

	if action.Action != ActionNoChange {
		t.Errorf("processBookmark() action = %v, want ActionNoChange", action.Action)
	}
	if !errors.Is(action.Error, errUnverifiable) {
		t.Errorf("action.Error = %v, want errUnverifiable", action.Error)
	}
	if chk.stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", chk.stats.Skipped)
	}
	if chk.stats.ActionDelete != 0 {
		t.Errorf("ActionDelete = %d, want 0 for an unreachable host", chk.stats.ActionDelete)
	}
}

// A redirect whose Location points at mailto: or ftp: is not a destination
// this tool can verify, and must not become the bookmark.
func TestFollowRedirects_RejectsNonHTTPRedirect(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "mailto:someone@example.com")
			w.WriteHeader(http.StatusMovedPermanently)
		}))
	t.Cleanup(server.Close)

	if _, err := newTestChecker(t).followRedirects(server.URL); !errors.Is(err, errUnverifiable) {
		t.Errorf("followRedirects() error = %v, want errUnverifiable", err)
	}
}

// When a host refuses HEAD and the GET retry also fails, the HEAD status stands
// rather than the failure being swallowed into a success.
func TestConfirmWithGet_KeepsHeadStatusWhenGetFails(t *testing.T) {
	t.Parallel()

	got := newTestChecker(t).confirmWithGet(unreachableURL(t), http.StatusForbidden)
	if got != http.StatusForbidden {
		t.Errorf("confirmWithGet() = %d, want the original HEAD status", got)
	}
}

// The GET re-check follows redirects, so the private-address guard runs on each
// hop: a host that refuses HEAD could otherwise steer the GET at the user's own
// network. The refused redirect is a failed GET, so the HEAD status stands.
func TestConfirmWithGet_RefusesRedirectToNonPublicHost(t *testing.T) {
	t.Parallel()

	privateRequests := 0
	private := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			privateRequests++
			w.WriteHeader(http.StatusNotFound)
		}))
	t.Cleanup(private.Close)

	public := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, private.URL+"/", http.StatusFound)
		}))
	t.Cleanup(public.Close)

	chk := newTestChecker(t)
	chk.isPublicHost = func(rawURL string) bool { return !strings.HasPrefix(rawURL, private.URL) }

	got := chk.confirmWithGet(public.URL+"/page", http.StatusForbidden)
	if got != http.StatusForbidden {
		t.Errorf("confirmWithGet() = %d, want the HEAD status %d", got, http.StatusForbidden)
	}
	if privateRequests != 0 {
		t.Errorf("the non-public host got %d requests, want none", privateRequests)
	}
}

// A redirect chain longer than maxRedirectHops stops at the limit, the same
// request budget followRedirects spends, and the last redirect answer is the
// status reported.
func TestConfirmWithGet_StopsAtHopLimit(t *testing.T) {
	t.Parallel()

	var server *httptest.Server

	requests := 0
	server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			requests++
			http.Redirect(w, r, server.URL+"/next", http.StatusFound)
		}))
	t.Cleanup(server.Close)

	got := newTestChecker(t).confirmWithGet(server.URL, http.StatusForbidden)
	if got != http.StatusFound {
		t.Errorf("confirmWithGet() = %d, want the last redirect's %d", got, http.StatusFound)
	}
	if requests != maxRedirectHops {
		t.Errorf("issued %d requests, want %d", requests, maxRedirectHops)
	}
}

// A 403 the GET retry also gets is still not a dead link. The GET retry exists
// for bot protection, but it sends Go's default User-Agent and no JS, so it is
// answered 403 by exactly the protection it was written to see past — which
// made a confirmed 403 a deletion of a live page.
func TestCheckURL_ConfirmedForbiddenIsUnverifiable(t *testing.T) {
	t.Parallel()

	server := staticServer(t, http.StatusForbidden, "")

	_, _, err := newTestChecker(t).checkURL(server.URL, "test context")
	if !errors.Is(err, errUnverifiable) {
		t.Errorf("checkURL() error = %v, want errUnverifiable when GET also refuses", err)
	}
	if errors.Is(err, errDeadLink) {
		t.Error("a refused request must not be reported as a dead link")
	}
}

// GitHub answers an unauthenticated request for a private repository with 404,
// so on that host a 404 is no evidence the page is gone. The httptest host is
// registered in its place; the bookmark must be skipped and counted, not
// deleted.
func TestCheckURL_NotFoundFromMaskingHostIsUnverifiable(t *testing.T) {
	t.Parallel()

	server := staticServer(t, http.StatusNotFound, "")

	chk := newTestChecker(t)
	chk.notFoundMasksPrivate = map[string]bool{"127.0.0.1": true}

	action := chk.processBookmark(Bookmark{URL: server.URL + "/org/private-repo"})

	if action.Action == ActionDelete {
		t.Fatal("a 404 from a host that masks private resources deleted the bookmark")
	}
	if !errors.Is(action.Error, errUnverifiable) {
		t.Errorf("error = %v, want errUnverifiable", action.Error)
	}
	if chk.stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", chk.stats.Skipped)
	}
}

// The production set names github.com; a 404 from any other host still deletes.
func TestNotFoundMasksPrivate_NamesGitHub(t *testing.T) {
	t.Parallel()

	masks := newTestChecker(t).notFoundMasksPrivate
	if !masks["github.com"] {
		t.Error("github.com must be listed: it answers 404 for private repositories")
	}
	if masks["gist.github.com"] {
		t.Error("gist.github.com must not be listed: a secret gist opens by URL")
	}
}

// The status set that may delete a bookmark, pinned. Only an answer about the
// resource counts; every other 4xx describes the request and must be skipped.
func TestCheckURL_OnlyGoneStatusesDelete(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			server := staticServer(t, testCase.status, "")

			_, _, err := newTestChecker(t).checkURL(server.URL, "test context")
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
func TestFixParenthesesSuffix_UnreachableTrimmedURLKeepsOriginal(t *testing.T) {
	t.Parallel()

	original := unreachableURL(t) + "/page)"
	chk := newTestChecker(t)

	if got := chk.fixParenthesesSuffix(original); got != original {
		t.Errorf("fixParenthesesSuffix() = %q, want the original %q", got, original)
	}
	if chk.stats.ParenthesesFixed != 0 {
		t.Errorf("ParenthesesFixed = %d, want 0", chk.stats.ParenthesesFixed)
	}
}

// A failed delete of the stale URL leaves a duplicate behind, so it must be
// counted even though the add succeeded.
func TestApplyUpdateWrite_FailedOldURLDeleteIsCounted(t *testing.T) {
	t.Parallel()

	server := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)

		if strings.HasPrefix(r.URL.Path, "/posts/delete") {
			writeBody(t, w, `{"result_code":"item not found"}`)

			return
		}

		writeBody(t, w, testDoneBody)
	})

	chk := newWriteChecker(t, server.URL)

	chk.applyUpdateWrite(BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
	})

	// The add landed, so the update counts; the delete did not, so the failure
	// counts too. Reporting only one of the two would misstate the outcome.
	if chk.stats.UpdatesPerformed != 1 {
		t.Errorf("UpdatesPerformed = %d, want 1", chk.stats.UpdatesPerformed)
	}
	if chk.stats.ApplyErrors != 1 {
		t.Errorf("ApplyErrors = %d, want 1 for the failed delete", chk.stats.ApplyErrors)
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
func TestFollowRedirects_RejectsPrivateRedirectTarget(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
			w.WriteHeader(http.StatusFound)
		}))
	t.Cleanup(server.Close)

	chk := newTestChecker(t)
	chk.isPublicHost = isPublicHostImpl

	_, err := chk.followRedirects(server.URL)
	if !errors.Is(err, errUnverifiable) {
		t.Errorf("followRedirects() error = %v, want errUnverifiable", err)
	}
	if errors.Is(err, errDeadLink) {
		t.Error("a redirect into private space must not be reported as a dead link")
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

	resp, err := client.Do(request)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer closeResponse(t, resp)

	if seen != "caller-chosen/1.0" {
		t.Errorf("User-Agent = %q, want the caller's own", seen)
	}
}

// The worker pool sends the redirect client's first requests from many
// goroutines at once. The User-Agent used to be a field written lazily inside
// RoundTrip, and that write raced; under -race this test fails if it comes back.
func TestUserAgentTransport_ConcurrentFirstUse(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex

	agents := map[string]int{}

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			agents[r.UserAgent()]++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(server.Close)

	client := &http.Client{Transport: newUserAgentTransport()}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Errorf("request failed: %v", err)

				return
			}
			closeResponse(t, resp)
		})
	}
	wg.Wait()

	if len(agents) != 1 || agents[userAgent()] != 8 {
		t.Errorf("User-Agents seen = %v, want 8 requests all carrying %q", agents, userAgent())
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

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer closeResponse(t, resp)

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
func TestHostThrottle_SpacesRequestsToOneHost(t *testing.T) {
	t.Parallel()

	const interval = 20 * time.Millisecond

	hosts := newHostThrottle(interval)
	server := staticServer(t, http.StatusOK, "")

	start := time.Now()
	for range 3 {
		hosts.wait(server.URL)
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
func TestHostThrottle_DoesNotSpaceDistinctHosts(t *testing.T) {
	t.Parallel()

	hosts := newHostThrottle(time.Hour)
	first := staticServer(t, http.StatusOK, "")
	second := staticServer(t, http.StatusOK, "")

	start := time.Now()
	hosts.wait(first.URL)
	hosts.wait(second.URL)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("two distinct hosts took %s; they must not share a slot", elapsed)
	}
}

// A URL that resolves onto a bookmark the account already holds must not be
// written: posts/add with replace=yes overwrites the destination entirely, so
// the existing bookmark's notes and tags would be replaced by the mover's.
// Only the duplicate is removed.
func TestApplyUpdateWrite_ExistingTargetIsMergedNotOverwritten(t *testing.T) {
	t.Parallel()

	server := okServer(t)
	chk := newWriteChecker(t, server.URL)
	chk.knownURLs = map[string]bool{testURL: true, testNewURL: true}
	// Verbose so the merge's own report line runs; a silent merge would leave
	// the user unable to tell a collapsed duplicate from a plain update.
	chk.verbose = true

	chk.applyUpdateAction(BookmarkAction{
		Original: Bookmark{URL: testURL, Title: "mover", Tags: "mover-tag"},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
	})

	if printed := stdoutOf(chk); !strings.Contains(printed, "Merged:") {
		t.Errorf("verbose output = %q, want it to report the merge", printed)
	}

	for _, request := range server.Requests() {
		if strings.HasPrefix(request, "/posts/add") {
			t.Errorf("posts/add was called for a URL the account already holds: %s", request)
		}
	}

	if chk.stats.Merged != 1 {
		t.Errorf("Merged = %d, want 1", chk.stats.Merged)
	}
	if chk.stats.UpdatesPerformed != 0 {
		t.Errorf("UpdatesPerformed = %d, want 0; nothing was written", chk.stats.UpdatesPerformed)
	}
	if chk.stats.ApplyErrors != 0 {
		t.Errorf("ApplyErrors = %d, want 0", chk.stats.ApplyErrors)
	}
}

// A merge whose delete Pinboard refuses leaves the duplicate in place, so it
// must be counted as an apply error — that count is what makes the run exit
// non-zero, and a silently failed merge would report a clean run.
func TestApplyUpdateWrite_FailedMergeDeleteIsCounted(t *testing.T) {
	t.Parallel()

	chk := newWriteChecker(t,
		staticServer(t, http.StatusOK, `{"result_code":"item not found"}`).URL)
	chk.knownURLs = map[string]bool{testURL: true, testNewURL: true}

	if outcome := chk.applyUpdateWrite(BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
	}); outcome != outcomeFailed {
		t.Errorf("applyUpdateWrite() = %v, want outcomeFailed", outcome)
	}

	if chk.stats.ApplyErrors != 1 {
		t.Errorf("ApplyErrors = %d, want 1 for the failed merge delete", chk.stats.ApplyErrors)
	}
	if chk.stats.Merged != 0 {
		t.Errorf("Merged = %d, want 0; the duplicate is still there", chk.stats.Merged)
	}
}

// A destination the account does not hold is still written normally; the merge
// path must not swallow ordinary URL rewrites.
func TestApplyUpdateWrite_NewTargetIsWritten(t *testing.T) {
	t.Parallel()

	chk := newWriteChecker(t, okServer(t).URL)
	chk.knownURLs = map[string]bool{testURL: true}

	chk.applyUpdateWrite(BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
	})

	if chk.stats.UpdatesPerformed != 1 {
		t.Errorf("UpdatesPerformed = %d, want 1", chk.stats.UpdatesPerformed)
	}
	if chk.stats.Merged != 0 {
		t.Errorf("Merged = %d, want 0", chk.stats.Merged)
	}
	// The written URL now counts as held, so a later mover to it merges.
	if !chk.knownURLs[testNewURL] {
		t.Error("knownURLs lacks the URL just written")
	}
}

// Pinboard answers some endpoints with "result" rather than "result_code".
func TestDecodePinboardResult_AcceptsResultField(t *testing.T) {
	t.Parallel()

	chk := newWriteChecker(t, staticServer(t, http.StatusOK, `{"result":"done"}`).URL)

	if err := chk.deleteBookmark(testURL); err != nil {
		t.Errorf("deleteBookmark() error = %v, want nil for a `result` document", err)
	}
}

// A body that is not the expected document must fail rather than be read as a
// success: silently accepting garbage is how a rejected write looked applied.
func TestPinboardResponses_RejectMalformedJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		call func(*checker) error
	}{
		{
			name: "write result",
			body: "not json at all",
			call: func(chk *checker) error { return chk.deleteBookmark(testURL) },
		},
		{
			name: "bookmark list",
			body: `{"not":"an array"}`,
			call: func(chk *checker) error {
				_, err := chk.getBookmarks()

				return err
			},
		},
		{
			name: "tag suggestions",
			body: "<suggested><popular>go</popular></suggested>",
			call: func(chk *checker) error {
				_, err := chk.getSuggestedTags(testURL)

				return err
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			chk := newWriteChecker(t, staticServer(t, http.StatusOK, testCase.body).URL)

			if err := testCase.call(chk); err == nil {
				t.Error("error = nil, want a decode failure")
			}
		})
	}
}

// A failed lookup must yield no value and be counted as a failure. Returning a
// partial or garbage title would write it to the bookmark.
func TestFetchIfNeeded_FailureYieldsNothingAndCounts(t *testing.T) {
	t.Parallel()

	t.Run("title", func(t *testing.T) {
		t.Parallel()

		chk := newTestChecker(t)
		server := staticServer(t, http.StatusNotFound, "")

		if got := chk.fetchTitleIfNeeded(Bookmark{URL: server.URL}, ""); got != "" {
			t.Errorf("fetchTitleIfNeeded() = %q, want empty on failure", got)
		}
		if chk.stats.TitlesFailed != 1 {
			t.Errorf("TitlesFailed = %d, want 1", chk.stats.TitlesFailed)
		}
		if chk.stats.TitlesFetched != 0 {
			t.Errorf("TitlesFetched = %d, want 0", chk.stats.TitlesFetched)
		}
	})

	t.Run("tags", func(t *testing.T) {
		t.Parallel()

		chk := newWriteChecker(t, staticServer(t, http.StatusNotFound, "").URL)

		if got := chk.fetchTagsIfNeeded(Bookmark{URL: testURL}, ""); got != "" {
			t.Errorf("fetchTagsIfNeeded() = %q, want empty on failure", got)
		}
		if chk.stats.TagsFailed != 1 {
			t.Errorf("TagsFailed = %d, want 1", chk.stats.TagsFailed)
		}
		if chk.stats.TagsFetched != 0 {
			t.Errorf("TagsFetched = %d, want 0", chk.stats.TagsFetched)
		}
	})
}

// A call that succeeded and had nothing to take is not a failure. Both are
// ordinary: most pages Pinboard has no tag suggestions for, and a page may
// carry no <title> at all. Counting them as failures made a run where every
// response was 200 report hundreds of them.
func TestFetchIfNeeded_EmptyResultIsNotAFailure(t *testing.T) {
	t.Parallel()

	t.Run("tags with no suggestions", func(t *testing.T) {
		t.Parallel()

		chk := newWriteChecker(t, staticServer(t, http.StatusOK, `[]`).URL)

		if got := chk.fetchTagsIfNeeded(Bookmark{URL: testURL}, ""); got != "" {
			t.Errorf("fetchTagsIfNeeded() = %q, want empty when nothing is suggested", got)
		}
		if chk.stats.TagsEmpty != 1 {
			t.Errorf("TagsEmpty = %d, want 1", chk.stats.TagsEmpty)
		}
		if chk.stats.TagsFailed != 0 {
			t.Errorf("TagsFailed = %d, want 0; the call succeeded", chk.stats.TagsFailed)
		}
	})

	t.Run("title on a page without one", func(t *testing.T) {
		t.Parallel()

		chk := newTestChecker(t)
		server := staticServer(t, http.StatusOK, "<html><body>no title here</body></html>")

		if got := chk.fetchTitleIfNeeded(Bookmark{URL: server.URL}, ""); got != "" {
			t.Errorf("fetchTitleIfNeeded() = %q, want empty when the page has no title", got)
		}
		if chk.stats.TitlesEmpty != 1 {
			t.Errorf("TitlesEmpty = %d, want 1", chk.stats.TitlesEmpty)
		}
		if chk.stats.TitlesFailed != 0 {
			t.Errorf("TitlesFailed = %d, want 0; the fetch succeeded", chk.stats.TitlesFailed)
		}
	})
}

// A connection that dies mid-body must fail rather than yield whatever was
// parsed from the fragment received so far, which would be written to the
// bookmark as its title.
func TestGetPageTitle_TruncatedResponse(t *testing.T) {
	t.Parallel()

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

	if _, err := newTestChecker(t).getPageTitle(server.URL); err == nil {
		t.Error("getPageTitle() error = nil, want a failure on a truncated body")
	}
}

// discardLogger silences the httptest server's own logging for tests that
// deliberately abort a response.
func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestGetPageTitle_TransportFailure(t *testing.T) {
	t.Parallel()

	if _, err := newTestChecker(t).getPageTitle(unreachableURL(t)); err == nil {
		t.Error("getPageTitle() error = nil, want a transport error")
	}
}

// getPageTitle must reject a body that is not parseable as a document rather
// than write whatever it extracted into the bookmark title.
func TestGetPageTitle_UnparseableContentType(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			// A media type with an illegal parameter cannot be parsed.
			w.Header().Set("Content-Type", "text/html; charset")
			w.WriteHeader(http.StatusOK)
			writeBody(t, w, "<html><head><title>x</title></head></html>")
		}))
	t.Cleanup(server.Close)

	if _, err := newTestChecker(t).getPageTitle(server.URL); err == nil {
		t.Error("getPageTitle() error = nil, want an unparseable Content-Type error")
	}
}

// updateBookmark prefers freshly fetched tags over the bookmark's existing set.
func TestUpdateBookmark_PrefersNewTags(t *testing.T) {
	t.Parallel()

	rec := okServer(t)
	chk := newWriteChecker(t, rec.URL)

	bookmark := Bookmark{URL: testURL, Tags: "old"}
	if err := chk.updateBookmark(bookmark, testURL, "", "fresh,tags"); err != nil {
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
