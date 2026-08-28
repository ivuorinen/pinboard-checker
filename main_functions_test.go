// main_functions_test.go covers the pure decision helpers, URL checking, and
// the Pinboard read and write calls.
package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestShouldUpdateBookmark(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		bookmark Bookmark
		newURL   string
		newTitle string
		newTags  string
		want     bool
	}{
		{
			name:     "no changes",
			bookmark: Bookmark{URL: testURL, Title: "Title", Tags: testTag},
			want:     false,
		},
		{
			name:     "URL changed",
			bookmark: Bookmark{URL: testURL},
			newURL:   testNewURL,
			want:     true,
		},
		{
			name:     "same URL not counted as change",
			bookmark: Bookmark{URL: testURL},
			newURL:   testURL,
			want:     false,
		},
		{
			name:     "title added",
			bookmark: Bookmark{URL: testURL},
			newTitle: testTitle,
			want:     true,
		},
		{
			name:     "tags added",
			bookmark: Bookmark{URL: testURL},
			newTags:  "tag1,tag2",
			want:     true,
		},
		{
			name:     "multiple changes",
			bookmark: Bookmark{URL: testURL},
			newURL:   testNewURL,
			newTitle: testTitle,
			newTags:  "tag1",
			want:     true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := shouldUpdateBookmark(
				testCase.bookmark,
				testCase.newURL,
				testCase.newTitle,
				testCase.newTags,
			)
			if got != testCase.want {
				t.Errorf("shouldUpdateBookmark() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestIsCheckableURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "http", input: "http://example.com", want: true},
		{name: "https", input: "https://example.com", want: true},
		// Pinboard stores all four of these; http.Head cannot dial any of them,
		// and treating that as a dead link deleted them.
		{name: "mailto", input: "mailto:someone@example.com", want: false},
		{name: "ftp", input: testFTPURL, want: false},
		{name: "file", input: "file:///etc/hosts", want: false},
		{name: "javascript bookmarklet", input: "javascript:void(0)", want: false},
		{name: "empty", input: "", want: false},
		// url.Parse itself fails here, so the scheme can never be checked.
		{name: "unparseable", input: "http://[::1", want: false},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := isCheckableURL(testCase.input); got != testCase.want {
				t.Errorf("isCheckableURL(%q) = %v, want %v", testCase.input, got, testCase.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()

	if got := truncateRunes("short", 10); got != "short" {
		t.Errorf("truncateRunes() = %q, want it unchanged", got)
	}

	got := truncateRunes(strings.Repeat("ä", 20), 5)
	if runes := []rune(got); len(runes) != 5 {
		t.Errorf("truncateRunes() kept %d runes, want 5", len(runes))
	}
	if !isValidUTF8(got) {
		t.Error("truncateRunes() produced invalid UTF-8; it must cut on rune boundaries")
	}
}

func isValidUTF8(value string) bool {
	for _, r := range value {
		if r == '�' {
			return false
		}
	}

	return true
}

func TestRequireHTML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		wantErr     bool
	}{
		{name: "html", contentType: "text/html; charset=utf-8"},
		{name: "xhtml", contentType: "application/xhtml+xml"},
		{name: "absent header is allowed", contentType: ""},
		{name: "video is rejected", contentType: "video/mp4", wantErr: true},
		{name: "octet stream is rejected", contentType: "application/octet-stream", wantErr: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := requireHTML(testCase.contentType)
			if (err != nil) != testCase.wantErr {
				t.Errorf("requireHTML(%q) error = %v, wantErr %v",
					testCase.contentType, err, testCase.wantErr)
			}
		})
	}
}

func TestCountActionsToApply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		actions []BookmarkAction
		want    int
	}{
		{name: "empty actions", actions: []BookmarkAction{}, want: 0},
		{
			name: "only no-change actions",
			actions: []BookmarkAction{
				{Action: ActionNoChange},
				{Action: ActionNoChange},
			},
			want: 0,
		},
		{
			name: "mixed actions",
			actions: []BookmarkAction{
				{Action: ActionNoChange},
				{Action: ActionUpdate},
				{Action: ActionDelete},
				{Action: ActionNoChange},
				{Action: ActionUpdate},
			},
			want: 3,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := countActionsToApply(testCase.actions); got != testCase.want {
				t.Errorf("countActionsToApply() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestValidateFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		token   string
		workers int
		timeout time.Duration
		want    int
	}{
		{name: "valid", token: testToken, workers: 10, timeout: time.Second, want: 0},
		{name: "missing token", token: "", workers: 10, timeout: time.Second, want: 1},
		// Zero produced an unbuffered semaphore and deadlocked the dispatch
		// loop; a negative value panicked inside make.
		{name: "zero workers", token: testToken, workers: 0, timeout: time.Second, want: 1},
		{name: "negative workers", token: testToken, workers: -1, timeout: time.Second, want: 1},
		{
			name:    "too many workers",
			token:   testToken,
			workers: maxWorkers + 1,
			timeout: time.Second,
			want:    1,
		},
		{name: "zero timeout", token: testToken, workers: 10, timeout: 0, want: 1},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := validateFlags(testCase.token, testCase.workers, testCase.timeout)
			if got != testCase.want {
				t.Errorf("validateFlags() = %d, want %d", got, testCase.want)
			}
		})
	}
}

//nolint:paralleltest // writes to the stats singleton via RecordHTTPStatus
func TestValidateURLAccessibility(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    error
	}{
		{name: "200 OK", statusCode: http.StatusOK},
		{name: "301 Redirect", statusCode: http.StatusMovedPermanently},
		// 5xx is temporary; the bookmark is kept for a later run.
		{name: "500 Server Error", statusCode: http.StatusInternalServerError},
		{name: "404 Not Found", statusCode: http.StatusNotFound, wantErr: errDeadLink},
		{name: "410 Gone", statusCode: http.StatusGone, wantErr: errDeadLink},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resetStats(t)

			server := staticServer(t, testCase.statusCode, "")

			err := validateURLAccessibility(server.URL, "test context")
			switch {
			case testCase.wantErr == nil && err != nil:
				t.Errorf("validateURLAccessibility() error = %v, want nil", err)
			case testCase.wantErr != nil && !errors.Is(err, testCase.wantErr):
				t.Errorf("validateURLAccessibility() error = %v, want %v", err, testCase.wantErr)
			}
		})
	}
}

// A host that refuses HEAD is not a dead link. Deleting on the HEAD status
// destroyed live bookmarks behind bot protection.
//
//nolint:paralleltest // writes to the stats singleton
func TestValidateURLAccessibility_RetriesRefusedHeadWithGet(t *testing.T) {
	resetStats(t)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)

				return
			}
			w.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(server.Close)

	if err := validateURLAccessibility(server.URL, "test context"); err != nil {
		t.Errorf("validateURLAccessibility() error = %v, want nil when GET succeeds", err)
	}
}

// A transport failure must be errUnverifiable, never errDeadLink: a timeout is
// this tool's limitation, not evidence the bookmark is gone.
//
//nolint:paralleltest // writes to the stats singleton
func TestValidateURLAccessibility_TransportFailureIsUnverifiable(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusOK, "")
	unreachable := server.URL
	server.Close()

	err := validateURLAccessibility(unreachable, "test context")
	if !errors.Is(err, errUnverifiable) {
		t.Errorf("error = %v, want errUnverifiable", err)
	}
	if errors.Is(err, errDeadLink) {
		t.Error("a transport failure must not be reported as a dead link")
	}
}

//nolint:paralleltest // writes to the stats singleton
func TestExpandAndCheckURL(t *testing.T) {
	resetStats(t)

	t.Run("returns the URL unchanged when nothing rewrites it", func(t *testing.T) {
		server := staticServer(t, http.StatusOK, "")

		got, err := expandAndCheckURL(server.URL, false)
		if err != nil {
			t.Fatalf("expandAndCheckURL() error = %v, want nil", err)
		}
		if got != server.URL {
			t.Errorf("expandAndCheckURL() = %q, want %q", got, server.URL)
		}
	})

	t.Run("reports a 404 as a dead link", func(t *testing.T) {
		server := staticServer(t, http.StatusNotFound, "")

		_, err := expandAndCheckURL(server.URL, false)
		if !errors.Is(err, errDeadLink) {
			t.Errorf("expandAndCheckURL() error = %v, want errDeadLink", err)
		}
	})
}

// The common path — no shortener, no parenthesis, no redirect — used to issue
// four requests for the same URL because every step re-validated regardless.
//
//nolint:paralleltest // writes to the stats singleton
func TestExpandAndCheckURL_DoesNotRevalidateUnchangedURL(t *testing.T) {
	resetStats(t)

	rec := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	if _, err := expandAndCheckURL(rec.URL, false); err != nil {
		t.Fatalf("expandAndCheckURL() error = %v, want nil", err)
	}

	// One validation plus the redirect probe.
	if got := len(rec.Requests()); got > 2 {
		t.Errorf("issued %d requests for an unchanged URL, want at most 2", got)
	}
}

//nolint:paralleltest // writes to the stats singleton
func TestFixParenthesesSuffix(t *testing.T) {
	resetStats(t)

	t.Run("drops the parenthesis when the trimmed URL works", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.RequestURI, ")") {
					w.WriteHeader(http.StatusBadRequest)

					return
				}
				w.WriteHeader(http.StatusOK)
			}))
		t.Cleanup(server.Close)

		withParen := server.URL + "/page)"

		got, err := fixParenthesesSuffix(withParen, false)
		if err != nil {
			t.Fatalf("fixParenthesesSuffix() error = %v, want nil", err)
		}
		if got != server.URL+"/page" {
			t.Errorf("fixParenthesesSuffix() = %q, want the trimmed URL", got)
		}
	})

	t.Run("keeps the parenthesis when trimming does not help", func(t *testing.T) {
		server := staticServer(t, http.StatusNotFound, "")
		withParen := server.URL + "/page)"

		got, err := fixParenthesesSuffix(withParen, false)
		if err != nil {
			t.Fatalf("fixParenthesesSuffix() error = %v, want nil", err)
		}
		if got != withParen {
			t.Errorf("fixParenthesesSuffix() = %q, want %q", got, withParen)
		}
	})

	t.Run("leaves a URL without a parenthesis alone", func(t *testing.T) {
		got, err := fixParenthesesSuffix(testURL, false)
		if err != nil {
			t.Fatalf("fixParenthesesSuffix() error = %v, want nil", err)
		}
		if got != testURL {
			t.Errorf("fixParenthesesSuffix() = %q, want %q", got, testURL)
		}
	})
}

//nolint:paralleltest // writes to the stats singleton
func TestCheckURL_Redirects(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		location string
		// want is resolved against the server URL when relative is true.
		want     string
		relative bool
	}{
		{
			name:     "301",
			status:   http.StatusMovedPermanently,
			location: testFinalURL,
			want:     testFinalURL,
		},
		{
			name:     "302",
			status:   http.StatusFound,
			location: testFinalURL,
			want:     testFinalURL,
		},
		// 303, 307, and 308 were previously ignored entirely.
		{
			name:     "303",
			status:   http.StatusSeeOther,
			location: testFinalURL,
			want:     testFinalURL,
		},
		{
			name:     "307",
			status:   http.StatusTemporaryRedirect,
			location: testFinalURL,
			want:     testFinalURL,
		},
		{
			name:     "308",
			status:   http.StatusPermanentRedirect,
			location: testFinalURL,
			want:     testFinalURL,
		},
		{
			// The raw header used to be stored verbatim, so "/moved" became the
			// bookmark URL and the bookmark was then deleted as unreachable.
			name:     "relative Location is resolved",
			status:   http.StatusMovedPermanently,
			location: "/moved",
			relative: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resetStats(t)

			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Location", testCase.location)
					w.WriteHeader(testCase.status)
				}))
			t.Cleanup(server.Close)

			want := testCase.want
			if testCase.relative {
				want = server.URL + testCase.location
			}

			got, err := checkURL(server.URL, "test context")
			if err != nil {
				t.Fatalf("checkURL() error = %v, want nil", err)
			}
			if got != want {
				t.Errorf("checkURL() redirect = %q, want %q", got, want)
			}
		})
	}
}

// An empty redirect means the URL is final.
//
//nolint:paralleltest // writes to the stats singleton
func TestCheckURL_NoRedirect(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusOK, "")

	got, err := checkURL(server.URL, "test context")
	if err != nil {
		t.Fatalf("checkURL() error = %v, want nil", err)
	}
	if got != "" {
		t.Errorf("checkURL() redirect = %q, want empty for a final URL", got)
	}
	if stats.Redirects != 0 {
		t.Errorf("recorded %d redirects, want 0", stats.Redirects)
	}
}

// A redirect status with no Location header leaves the URL final rather than
// erroring.
//
//nolint:paralleltest // writes to the stats singleton
func TestCheckURL_RedirectWithoutLocation(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusMovedPermanently, "")

	got, err := checkURL(server.URL, "test context")
	if err != nil {
		t.Fatalf("checkURL() error = %v, want nil", err)
	}
	if got != "" {
		t.Errorf("checkURL() redirect = %q, want empty without a Location", got)
	}
}

// followRedirects walks a chain rather than resolving only the first hop, which
// left a bookmark pointing at an intermediate URL.
//
//nolint:paralleltest // writes to the stats singleton
func TestFollowRedirects_WalksChain(t *testing.T) {
	resetStats(t)

	final := staticServer(t, http.StatusOK, "")

	var server *httptest.Server

	hops := 0
	server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/one":
				hops++
				w.Header().Set("Location", server.URL+"/two")
				w.WriteHeader(http.StatusMovedPermanently)
			case "/two":
				hops++
				w.Header().Set("Location", final.URL)
				w.WriteHeader(http.StatusPermanentRedirect)
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
	t.Cleanup(server.Close)

	got, err := followRedirects(server.URL+"/one", false)
	if err != nil {
		t.Fatalf("followRedirects() error = %v, want nil", err)
	}
	if got != final.URL {
		t.Errorf("followRedirects() = %q, want the destination %q", got, final.URL)
	}
	if hops != 2 {
		t.Errorf("walked %d hops, want 2", hops)
	}
	if stats.Redirects != 2 {
		t.Errorf("recorded %d redirects, want 2", stats.Redirects)
	}
}

// A redirect cycle must stop at the hop limit rather than loop forever.
//
//nolint:paralleltest // writes to the stats singleton
func TestFollowRedirects_StopsAtHopLimit(t *testing.T) {
	resetStats(t)

	var server *httptest.Server

	requests := 0
	server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			requests++
			// Points at itself under a fresh path so every hop is distinct.
			w.Header().Set("Location", server.URL+"/next")
			w.WriteHeader(http.StatusFound)
		}))
	t.Cleanup(server.Close)

	if _, err := followRedirects(server.URL, false); err != nil {
		t.Fatalf("followRedirects() error = %v, want nil at the hop limit", err)
	}
	if requests > maxRedirectHops {
		t.Errorf("issued %d requests, want at most %d", requests, maxRedirectHops)
	}
}

//nolint:paralleltest // writes to the stats singleton
func TestFollowRedirects_ReportsDeadLink(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusNotFound, "")

	if _, err := followRedirects(server.URL, false); !errors.Is(err, errDeadLink) {
		t.Errorf("followRedirects() error = %v, want errDeadLink", err)
	}
}

//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestGetBookmarks_Success(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusOK,
		`[{"href":"http://example.com","description":"Test","tags":"tag1",`+
			`"extended":"notes","time":"2020-01-02T03:04:05Z","shared":"no","toread":"yes"}]`)
	useEndpoint(t, server.URL)

	bookmarks, err := getBookmarks(testToken)
	if err != nil {
		t.Fatalf("getBookmarks() error = %v, want nil", err)
	}
	if len(bookmarks) != 1 {
		t.Fatalf("got %d bookmarks, want 1", len(bookmarks))
	}

	got := bookmarks[0]
	if got.URL != testURL {
		t.Errorf("URL = %q, want %q", got.URL, testURL)
	}
	// These three exist so updateBookmark can hand them back to posts/add.
	if got.Time != "2020-01-02T03:04:05Z" {
		t.Errorf("Time = %q, want the creation timestamp", got.Time)
	}
	if got.Shared != "no" {
		t.Errorf("Shared = %q, want %q", got.Shared, "no")
	}
	if got.ToRead != "yes" {
		t.Errorf("ToRead = %q, want %q", got.ToRead, "yes")
	}
}

// A 401 used to reach the JSON decoder and surface as "failed to decode
// bookmarks JSON", pointing the operator at the wrong problem.
//
//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestGetBookmarks_InvalidTokenNamesTheToken(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusUnauthorized, "API requires authentication")
	useEndpoint(t, server.URL)

	_, err := getBookmarks("invalid-token")
	if err == nil {
		t.Fatal("getBookmarks() error = nil, want an error for an invalid token")
	}
	if !strings.Contains(err.Error(), tokenName) {
		t.Errorf("error = %v, want it to name %s", err, tokenName)
	}
}

// Pinboard's docs require clients to back off on 429 rather than keep sending.
//
//nolint:paralleltest // repoints pinboardAPIEndpoint and widens the rate limiter
func TestPinboardGet_BacksOffOn429(t *testing.T) {
	resetStats(t)

	previousInterval := rateLimitInterval
	t.Cleanup(func() {
		rateLimitInterval = previousInterval
		rateLimiter.Reset(previousInterval)
	})

	attempts := 0
	rec := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, `[]`)
	})
	useEndpoint(t, rec.URL)

	if _, err := getBookmarks(testToken); err != nil {
		t.Fatalf("getBookmarks() error = %v, want nil after a retried 429", err)
	}
	if attempts != 3 {
		t.Errorf("made %d attempts, want 3", attempts)
	}
	if rateLimitInterval <= previousInterval {
		t.Errorf("interval = %s, want it widened from %s", rateLimitInterval, previousInterval)
	}
}

//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestUpdateBookmark_PreservesMetadataAndTargetsTheNewURL(t *testing.T) {
	resetStats(t)

	rec := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, `{"result_code":"done"}`)
	})
	useEndpoint(t, rec.URL)

	bookmark := Bookmark{
		URL:    testURL,
		Title:  "Old",
		Notes:  "notes",
		Tags:   "x y",
		Time:   "2020-01-02T03:04:05Z",
		Shared: "no",
		ToRead: "yes",
	}

	if err := updateBookmark(testToken, bookmark, testNewURL, testTitle, ""); err != nil {
		t.Fatalf("updateBookmark() error = %v, want nil", err)
	}

	requests := rec.Requests()
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1: %v", len(requests), requests)
	}

	request := requests[0]
	// Omitting any of these made posts/add re-default it, which reset the
	// creation date, cleared the unread flag, and republished private bookmarks.
	for _, want := range []string{
		"dt=2020-01-02T03%3A04%3A05Z",
		"shared=no",
		"toread=yes",
		"replace=yes",
		"format=json",
		"tags=x%2Cy",
	} {
		if !strings.Contains(request, want) {
			t.Errorf("request %q is missing %q", request, want)
		}
	}
}

// Pinboard reports a rejected write in the body at HTTP 200, so a status-only
// check counted failures as successes.
//
//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestUpdateBookmark_RejectsInBandFailure(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusOK, `{"result_code":"something went wrong"}`)
	useEndpoint(t, server.URL)

	err := updateBookmark(testToken, Bookmark{URL: testURL}, testURL, "", "")
	if err == nil {
		t.Fatal("updateBookmark() error = nil, want an error for a rejected write")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("error = %v, want it to carry the Pinboard result code", err)
	}
}

//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestUpdateBookmark_Unauthorized(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusUnauthorized, "")
	useEndpoint(t, server.URL)

	if err := updateBookmark("invalid", Bookmark{URL: testURL}, testURL, "", ""); err == nil {
		t.Error("updateBookmark() error = nil, want an error for an invalid token")
	}
}

//nolint:paralleltest // repoints pinboardAPIEndpoint
func TestDeleteBookmark(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "success", status: http.StatusOK, body: `{"result_code":"done"}`},
		{
			// posts/delete answers an unknown URL with 200 and this code.
			name:    "item not found",
			status:  http.StatusOK,
			body:    `{"result_code":"item not found"}`,
			wantErr: true,
		},
		{name: "unauthorized", status: http.StatusUnauthorized, body: "", wantErr: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resetStats(t)

			server := staticServer(t, testCase.status, testCase.body)
			useEndpoint(t, server.URL)

			err := deleteBookmark(testToken, testURL)
			if (err != nil) != testCase.wantErr {
				t.Errorf("deleteBookmark() error = %v, wantErr %v", err, testCase.wantErr)
			}
		})
	}
}

//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestFetchTitleIfNeeded(t *testing.T) {
	tests := []struct {
		name       string
		bookmark   Bookmark
		skipTitles bool
		wantTitle  string
	}{
		{
			name:       "skips when skipTitles is set",
			bookmark:   Bookmark{Title: ""},
			skipTitles: true,
			wantTitle:  "",
		},
		{
			name:      "skips when a title already exists",
			bookmark:  Bookmark{Title: "Existing Title"},
			wantTitle: "",
		},
		{
			// TrimSpace means a whitespace-only title counts as missing.
			name:      "treats a whitespace-only title as missing",
			bookmark:  Bookmark{Title: "   "},
			wantTitle: "Fetched Title",
		},
		{
			// The only case that actually fetches; the old table had no such row.
			name:      "fetches when the title is empty",
			bookmark:  Bookmark{Title: ""},
			wantTitle: "Fetched Title",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resetStats(t)
			setSkipFlags(t, testCase.skipTitles, true)

			server := staticServer(t, http.StatusOK,
				"<html><head><title>Fetched Title</title></head></html>")

			bookmark := testCase.bookmark
			bookmark.URL = server.URL

			if got := fetchTitleIfNeeded(bookmark, ""); got != testCase.wantTitle {
				t.Errorf("fetchTitleIfNeeded() = %q, want %q", got, testCase.wantTitle)
			}
		})
	}
}

//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestFetchTitleIfNeeded_PrefersTheNewURL(t *testing.T) {
	resetStats(t)
	setSkipFlags(t, false, true)

	server := staticServer(t, http.StatusOK,
		"<html><head><title>New URL Title</title></head></html>")

	got := fetchTitleIfNeeded(Bookmark{URL: "http://old.example.com", Title: ""}, server.URL)
	if got != "New URL Title" {
		t.Errorf("fetchTitleIfNeeded() = %q, want the title from the new URL", got)
	}
}

//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestFetchTagsIfNeeded(t *testing.T) {
	tests := []struct {
		name         string
		bookmark     Bookmark
		skipAutoTags bool
		wantEmpty    bool
	}{
		{
			name:         "skips when skipAutoTags is set",
			bookmark:     Bookmark{},
			skipAutoTags: true,
			wantEmpty:    true,
		},
		{name: "skips when tags exist", bookmark: Bookmark{Tags: "existing"}, wantEmpty: true},
		// TrimSpace means whitespace-only tags count as missing. The previous
		// test asserted the opposite and passed only because its tag fetch went
		// to the live Pinboard API and failed.
		{
			name:      "treats whitespace-only tags as missing",
			bookmark:  Bookmark{Tags: "  "},
			wantEmpty: false,
		},
		{name: "fetches when there are no tags", bookmark: Bookmark{}, wantEmpty: false},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resetStats(t)
			setSkipFlags(t, true, testCase.skipAutoTags)

			server := staticServer(t, http.StatusOK, `[{"popular":["tag1"]}]`)
			useEndpoint(t, server.URL)

			bookmark := testCase.bookmark
			bookmark.URL = testURL

			got := fetchTagsIfNeeded(bookmark, testToken, "")
			if testCase.wantEmpty && got != "" {
				t.Errorf("fetchTagsIfNeeded() = %q, want empty", got)
			}
			if !testCase.wantEmpty && got == "" {
				t.Error("fetchTagsIfNeeded() = empty, want fetched tags")
			}
		})
	}
}

//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestProcessBookmark_NoChanges(t *testing.T) {
	resetStats(t)
	setSkipFlags(t, true, true)

	server := staticServer(t, http.StatusOK, "")

	action := processBookmark(Bookmark{URL: server.URL, Title: testTitleAlt, Tags: testTag}, testToken)
	if action.Action != ActionNoChange {
		t.Errorf("processBookmark() action = %v, want ActionNoChange", action.Action)
	}
}

//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestProcessBookmark_DeadLinkIsDeleted(t *testing.T) {
	resetStats(t)
	setSkipFlags(t, true, true)

	server := staticServer(t, http.StatusNotFound, "")

	action := processBookmark(Bookmark{URL: server.URL, Title: testTitleAlt}, testToken)
	if action.Action != ActionDelete {
		t.Errorf("processBookmark() action = %v, want ActionDelete", action.Action)
	}
	if !errors.Is(action.Error, errDeadLink) {
		t.Errorf("action.Error = %v, want errDeadLink", action.Error)
	}
}

// A bookmark whose scheme http.Head cannot dial must be left alone. Deleting
// these destroyed every saved bookmarklet and mailto: bookmark.
//
//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestProcessBookmark_UndialableSchemeIsSkipped(t *testing.T) {
	resetStats(t)
	setSkipFlags(t, true, true)

	for _, rawURL := range []string{
		"mailto:someone@example.com",
		testFTPURL,
		"file:///etc/hosts",
		"javascript:void(0)",
	} {
		action := processBookmark(Bookmark{URL: rawURL}, testToken)
		if action.Action != ActionNoChange {
			t.Errorf("processBookmark(%q) action = %v, want ActionNoChange", rawURL, action.Action)
		}
	}

	if stats.Skipped != 4 {
		t.Errorf("Skipped = %d, want 4", stats.Skipped)
	}
	if stats.ActionDelete != 0 {
		t.Errorf("ActionDelete = %d, want 0", stats.ActionDelete)
	}
}

// A dead bookmark must not pay for a title or tag fetch whose result is thrown
// away, and the tag fetch costs a rate-limiter slot.
//
//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestProcessBookmark_SkipsFetchesForDeadLinks(t *testing.T) {
	resetStats(t)
	setSkipFlags(t, false, false)

	rec := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	useEndpoint(t, rec.URL)

	action := processBookmark(Bookmark{URL: rec.URL, Title: "", Tags: ""}, testToken)
	if action.Action != ActionDelete {
		t.Fatalf("processBookmark() action = %v, want ActionDelete", action.Action)
	}
	if stats.TitlesFailed != 0 || stats.TagsFailed != 0 {
		t.Errorf("recorded %d title and %d tag fetches for a dead link, want 0",
			stats.TitlesFailed, stats.TagsFailed)
	}
}

//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestProcessBookmark_WithTitle(t *testing.T) {
	resetStats(t)
	setSkipFlags(t, false, true)

	server := staticServer(t, http.StatusOK,
		"<html><head><title>Test Title</title></head></html>")

	action := processBookmark(Bookmark{URL: server.URL, Title: "", Tags: "existing"}, testToken)
	if action.Action != ActionUpdate {
		t.Fatalf("processBookmark() action = %v, want ActionUpdate", action.Action)
	}
	if action.NewTitle != "Test Title" {
		t.Errorf("NewTitle = %q, want %q", action.NewTitle, "Test Title")
	}
}

// processBookmarksParallel had no test at all, which is why -workers 0 shipped.
//
//nolint:paralleltest // mutates the skip flags and the stats singleton
func TestProcessBookmarksParallel(t *testing.T) {
	tests := []struct {
		name      string
		bookmarks int
		workers   int
	}{
		{name: "single worker", bookmarks: 5, workers: 1},
		{name: "more workers than bookmarks", bookmarks: 2, workers: 10},
		{name: "no bookmarks", bookmarks: 0, workers: 4},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resetStats(t)
			setSkipFlags(t, true, true)

			setCIMode(t, true)

			server := staticServer(t, http.StatusOK, "")

			bookmarks := make([]Bookmark, testCase.bookmarks)
			for i := range bookmarks {
				bookmarks[i] = Bookmark{URL: server.URL, Title: "t", Tags: "x"}
			}

			actions := processBookmarksParallel(bookmarks, testToken, testCase.workers)
			if len(actions) != testCase.bookmarks {
				t.Errorf("got %d actions, want %d", len(actions), testCase.bookmarks)
			}
		})
	}
}

// TestVersionString covers the ldflags path: an empty `version` must fall back
// to build info rather than reporting an empty version, and a stamped one wins.
func TestVersionString(t *testing.T) { //nolint:paralleltest // mutates the package-level version
	original := version
	t.Cleanup(func() { version = original })

	version = "1.2.3"

	if got := versionString(); got != "1.2.3" {
		t.Errorf("versionString() = %q, want the stamped version", got)
	}

	version = ""

	if got := versionString(); got == "" {
		t.Error("versionString() = empty, want a build-info fallback")
	}
}
