// main_test.go holds the suite's shared fixtures and helpers alongside the
// tests for title extraction, tag suggestion, and URL shortener expansion.
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Shared fixtures. These strings appear across every test file; hoisting them
// keeps goconst quiet and makes the intent of each value explicit.
const (
	testURL      = "http://example.com"
	testNewURL   = "http://example.org"
	testFinalURL = "https://example.org/final"
	testAbsURL   = "https://example.com/x"
	testTitle    = "New Title"
	testTitleAlt = "Test"
	testToken    = "test-token"
	testTag      = "tag1"
	testTagAlt   = "code"
	testTagList  = "tags"
	testFTPURL   = "ftp://ftp.example.com/file"
)

// TestMain shortens the global rate limiter for the whole suite. The production
// interval is three seconds per Pinboard call, which would dominate the run
// time; these tests exercise the request logic, not the pacing.
func TestMain(m *testing.M) {
	rateLimitInterval = time.Millisecond
	rateLimiter.Reset(time.Millisecond)

	os.Exit(m.Run())
}

// swap sets *target to value for the duration of the test and restores the
// previous value afterwards.
//
// Written once because the capture-assign-restore ordering is easy to get
// subtly wrong, and a missed restore does not fail the test that caused it — it
// surfaces later as an unrelated failure or, when parallel tests are involved,
// as a data race.
func swap[T any](t *testing.T, target *T, value T) {
	t.Helper()

	previous := *target
	*target = value

	t.Cleanup(func() { *target = previous })
}

// resetStats gives a test its own counters. stats is a package-level singleton
// that every code path writes to, so without this a test would assert on totals
// accumulated by whatever ran before it.
func resetStats(t *testing.T) {
	t.Helper()
	swap(t, &stats, newStatistics())
}

// useEndpoint points pinboardAPIEndpoint at a test server for one test.
func useEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	swap(t, &pinboardAPIEndpoint, endpoint)
}

// useBitlyEndpoint points bitlyEndpoint at a test server for one test.
func useBitlyEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	swap(t, &bitlyEndpoint, endpoint)
}

// useIsGdEndpoint points isGdEndpoint at a test server for one test.
func useIsGdEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	swap(t, &isGdEndpoint, endpoint)
}

// setSkipFlags sets the two feature toggles for one test and restores them
// afterwards, so a test never inherits whatever a previous one left behind.
func setSkipFlags(t *testing.T, titles, autoTags bool) {
	t.Helper()
	swap(t, &skipTitles, titles)
	swap(t, &skipAutoTags, autoTags)
}

// setVerbose sets the verbose toggle for one test and restores it afterwards.
func setVerbose(t *testing.T, value bool) {
	t.Helper()
	swap(t, &verbose, value)
}

// setCIMode sets the CI toggle for one test and restores it afterwards.
func setCIMode(t *testing.T, value bool) {
	t.Helper()
	swap(t, &ciMode, value)
}

// recordingServer answers requests with a handler while recording what was
// asked of it, so a test can assert on the requests actually issued rather than
// only that the code under test did not panic.
type recordingServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []string
}

func newRecordingServer(t *testing.T, handler http.HandlerFunc) *recordingServer {
	t.Helper()

	rec := &recordingServer{}
	rec.Server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			rec.mu.Lock()
			rec.requests = append(rec.requests, r.URL.Path+"?"+r.URL.RawQuery)
			rec.mu.Unlock()

			handler(w, r)
		}))

	t.Cleanup(rec.Close)

	return rec
}

// Requests returns a copy of the recorded request lines.
func (rec *recordingServer) Requests() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()

	return append([]string(nil), rec.requests...)
}

// writeBody writes a response body and fails the test if the write fails.
func writeBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("failed to write response: %v", err)
	}
}

// staticServer serves one status and body for every request.
func staticServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			if body != "" {
				writeBody(t, w, body)
			}
		}))

	t.Cleanup(server.Close)

	return server
}

//nolint:paralleltest // mutates the shortener seam and the stats singleton
func TestExpandURL_Bitly(t *testing.T) {
	resetStats(t)

	swap(t, &unshortenBitly, func(_ string) (string, error) {
		return "https://expanded.example.com", nil
	})

	got, err := expandURL("https://bit.ly/test", false)
	if err != nil {
		t.Fatalf("expandURL() error = %v, want nil", err)
	}
	if got != "https://expanded.example.com" {
		t.Errorf("expandURL() = %q, want the expanded URL", got)
	}
	if stats.ShortURLsExpanded[svcBitly] != 1 {
		t.Errorf("expected one recorded bitly expansion, got %d",
			stats.ShortURLsExpanded[svcBitly])
	}
}

// A shortener that cannot resolve a link must not report an error: expandURL's
// error routed the bookmark to deletion, so a Bitly outage deleted bookmarks.
//
//nolint:paralleltest // mutates the shortener seam and the stats singleton
func TestExpandURL_ShortenerFailureKeepsOriginal(t *testing.T) {
	resetStats(t)

	// Any failure will do; the specific error is not asserted.
	swap(t, &unshortenIsGd, func(_ string) (string, error) { return "", errNoBitlyToken })

	const short = "https://is.gd/test"

	got, err := expandURL(short, false)
	if err != nil {
		t.Fatalf("expandURL() error = %v, want nil for a failed expansion", err)
	}
	if got != short {
		t.Errorf("expandURL() = %q, want the original URL %q", got, short)
	}
	if stats.ShortURLsFailed[svcIsGd] != 1 {
		t.Errorf("expected one recorded is.gd failure, got %d", stats.ShortURLsFailed[svcIsGd])
	}
}

func TestExpandURL_NoExpansionNeeded(t *testing.T) {
	t.Parallel()

	const regularURL = "https://example.com/page"

	got, err := expandURL(regularURL, false)
	if err != nil {
		t.Fatalf("expandURL() error = %v, want nil", err)
	}
	if got != regularURL {
		t.Errorf("expandURL() = %q, want %q", got, regularURL)
	}
}

// Serial: each case repoints the package-level isGdEndpoint.
//
//nolint:paralleltest // repoints isGdEndpoint
func TestUnshortenIsGdImpl(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{
			name:   "expands from the url field",
			status: http.StatusOK,
			body:   `{"url":"https://example.com/final"}`,
			want:   "https://example.com/final",
		},
		{
			// The old implementation returned this whole document as the URL.
			name:    "reports an is.gd error code",
			status:  http.StatusOK,
			body:    `{"errorcode":1,"errormessage":"unknown shorturl"}`,
			wantErr: true,
		},
		{
			name:    "rejects a non-200 response",
			status:  http.StatusInternalServerError,
			body:    "",
			wantErr: true,
		},
		{
			name:    "rejects an empty url field",
			status:  http.StatusOK,
			body:    `{"url":""}`,
			wantErr: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			server := staticServer(t, testCase.status, testCase.body)

			useIsGdEndpoint(t, server.URL)

			got, err := unshortenIsGdImpl("https://is.gd/abc")
			if (err != nil) != testCase.wantErr {
				t.Fatalf("unshortenIsGdImpl() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Errorf("unshortenIsGdImpl() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestUnshortenTinyURLImpl(t *testing.T) {
	t.Parallel()

	t.Run("follows the redirect", func(t *testing.T) {
		t.Parallel()

		const target = "https://example.com/final"

		server := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", target)
				w.WriteHeader(http.StatusMovedPermanently)
			}))
		t.Cleanup(server.Close)

		got, err := unshortenTinyURLImpl(server.URL)
		if err != nil {
			t.Fatalf("unshortenTinyURLImpl() error = %v, want nil", err)
		}
		if got != target {
			t.Errorf("unshortenTinyURLImpl() = %q, want %q", got, target)
		}
	})

	t.Run("errors when there is no redirect", func(t *testing.T) {
		t.Parallel()

		server := staticServer(t, http.StatusOK, "not a redirect")

		if _, err := unshortenTinyURLImpl(server.URL); err == nil {
			t.Error("unshortenTinyURLImpl() error = nil, want an error without a Location")
		}
	})
}

const testBitlyURL = "https://bit.ly/test"

//nolint:paralleltest // uses t.Setenv
func TestUnshortenBitlyImpl_RequiresToken(t *testing.T) {
	t.Setenv(bitlyTokenName, "")

	if _, err := unshortenBitlyImpl(testBitlyURL); err == nil {
		t.Error("unshortenBitlyImpl() error = nil, want an error without a token")
	}
}

//nolint:paralleltest // uses t.Setenv and repoints bitlyEndpoint
func TestUnshortenBitlyImpl_SendsBitlinkIDAndReadsLongURL(t *testing.T) {
	t.Setenv(bitlyTokenName, "bitly-token")

	var (
		gotAuth string
		gotBody string
	)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")

			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("failed to read request body: %v", err)
			}
			gotBody = string(body)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			writeBody(t, w, `{"long_url":"`+testAbsURL+`"}`)
		}))
	t.Cleanup(server.Close)

	useBitlyEndpoint(t, server.URL)

	got, err := unshortenBitlyImpl(testBitlyURL)
	if err != nil {
		t.Fatalf("unshortenBitlyImpl() error = %v, want nil", err)
	}
	if got != testAbsURL {
		t.Errorf("unshortenBitlyImpl() = %q, want %q", got, testAbsURL)
	}
	if gotAuth != "Bearer bitly-token" {
		t.Errorf("Authorization = %q, want a bearer token", gotAuth)
	}
	// bitlink_id carries host and path with no scheme; the old code sent a
	// short_url field the endpoint does not accept.
	if !strings.Contains(gotBody, `"bitlink_id":"bit.ly/test"`) {
		t.Errorf("request body = %q, want a bitlink_id field", gotBody)
	}
}

// The old code returned ("", nil) here, and the empty URL was then judged a
// dead link and the bookmark deleted.
//
//nolint:paralleltest // uses t.Setenv and repoints bitlyEndpoint
func TestUnshortenBitlyImpl_RejectsNon200(t *testing.T) {
	t.Setenv(bitlyTokenName, "bitly-token")

	server := staticServer(t, http.StatusForbidden, `{"message":"FORBIDDEN"}`)
	useBitlyEndpoint(t, server.URL)

	got, err := unshortenBitlyImpl(testBitlyURL)
	if err == nil {
		t.Error("unshortenBitlyImpl() error = nil, want an error on HTTP 403")
	}
	if got != "" {
		t.Errorf("unshortenBitlyImpl() = %q, want empty on error", got)
	}
}

func TestValidExpandedURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "absolute URL", input: testAbsURL, want: testAbsURL},
		{name: "trims whitespace", input: " https://example.com/x\n", want: testAbsURL},
		{name: "rejects empty", input: "", wantErr: true},
		{name: "rejects relative", input: "/just/a/path", wantErr: true},
		{name: "rejects an error page", input: "Error: no such short URL", wantErr: true},
		{name: "rejects an unparseable URL", input: "http://[::1", wantErr: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := validExpandedURL(testCase.input)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("validExpandedURL() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Errorf("validExpandedURL() = %q, want %q", got, testCase.want)
			}
		})
	}
}

//nolint:paralleltest // writes to the stats singleton via RecordHTTPStatus
func TestGetPageTitle(t *testing.T) {
	resetStats(t)

	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		want        string
		wantErr     bool
	}{
		{
			name:   "extracts the title",
			status: http.StatusOK,
			body:   `<html><head><title>Test Page Title</title></head><body></body></html>`,
			want:   "Test Page Title",
		},
		{
			name:   "trims whitespace",
			status: http.StatusOK,
			body:   `<html><head><title>  Trimmed Title  </title></head></html>`,
			want:   "Trimmed Title",
		},
		{
			name:   "no title element",
			status: http.StatusOK,
			body:   `<html><head></head><body></body></html>`,
			want:   "",
		},
		{
			name:   "empty title element",
			status: http.StatusOK,
			body:   `<html><head><title></title></head></html>`,
			want:   "",
		},
		{
			// FirstChild alone stopped at the entity and returned "A ".
			name:   "joins text split by an entity",
			status: http.StatusOK,
			body:   `<html><head><title>A &amp; B</title></head></html>`,
			want:   "A & B",
		},
		{
			// An inline SVG <title> is an icon label, not the page title.
			name:   "skips a title inside inline SVG",
			status: http.StatusOK,
			body: `<html><head><title>Real Title</title></head>` +
				`<body><svg><title>icon</title></svg></body></html>`,
			want: "Real Title",
		},
		{
			name:    "rejects a 404",
			status:  http.StatusNotFound,
			body:    "",
			wantErr: true,
		},
		{
			name:        "rejects a non-HTML body",
			status:      http.StatusOK,
			contentType: "video/mp4",
			body:        "not markup",
			wantErr:     true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					if testCase.contentType != "" {
						w.Header().Set("Content-Type", testCase.contentType)
					}
					w.WriteHeader(testCase.status)
					if testCase.body != "" {
						writeBody(t, w, testCase.body)
					}
				}))
			defer server.Close()

			got, err := getPageTitle(server.URL)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("getPageTitle() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Errorf("getPageTitle() = %q, want %q", got, testCase.want)
			}
		})
	}
}

//nolint:paralleltest // writes to the stats singleton
func TestGetPageTitle_TruncatesToPinboardLimit(t *testing.T) {
	resetStats(t)

	long := strings.Repeat("ä", maxPinboardTitleLen+50)
	server := staticServer(t, http.StatusOK, "<html><head><title>"+long+"</title></head></html>")

	got, err := getPageTitle(server.URL)
	if err != nil {
		t.Fatalf("getPageTitle() error = %v, want nil", err)
	}
	// Counted in runes: cutting on bytes would split the multi-byte character.
	if runes := []rune(got); len(runes) != maxPinboardTitleLen {
		t.Errorf("title length = %d runes, want %d", len(runes), maxPinboardTitleLen)
	}
}

//nolint:paralleltest // repoints pinboardAPIEndpoint and the stats singleton
func TestGetSuggestedTags(t *testing.T) {
	resetStats(t)

	tests := []struct {
		name     string
		status   int
		body     string
		want     string
		wantErr  bool
		contains []string
	}{
		{
			// Pinboard returns popular and recommended as separate elements;
			// reading only the first dropped every recommended tag.
			name:     "collects both array elements",
			status:   http.StatusOK,
			body:     `[{"popular":["go","golang"]},{"recommended":["code","tutorial"]}]`,
			contains: []string{"go", "golang", testTagAlt, "tutorial", autoTagMarker},
		},
		{
			name:     "collects a single combined element",
			status:   http.StatusOK,
			body:     `[{"popular":["go"],"recommended":["code"]}]`,
			contains: []string{"go", testTagAlt, autoTagMarker},
		},
		{
			// A bare marker made every untagged bookmark look changed.
			name:   "returns nothing when there are no suggestions",
			status: http.StatusOK,
			body:   `[]`,
			want:   "",
		},
		{
			name:    "reports a 404",
			status:  http.StatusNotFound,
			body:    "",
			wantErr: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			server := staticServer(t, testCase.status, testCase.body)
			useEndpoint(t, server.URL)

			got, err := getSuggestedTags(testToken, testURL)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("getSuggestedTags() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if testCase.contains == nil && got != testCase.want {
				t.Errorf("getSuggestedTags() = %q, want %q", got, testCase.want)
			}
			for _, tag := range testCase.contains {
				if !slices.Contains(strings.Split(got, ","), tag) {
					t.Errorf("getSuggestedTags() = %q, want it to contain tag %q", got, tag)
				}
			}
		})
	}
}

// getSuggestedTags must ask for JSON: Pinboard answers in XML by default, so
// omitting the parameter made every decode fail.
//
//nolint:paralleltest // repoints pinboardAPIEndpoint and the stats singleton
func TestGetSuggestedTags_RequestsJSON(t *testing.T) {
	resetStats(t)

	rec := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, `[{"popular":["go"]}]`)
	})
	useEndpoint(t, rec.URL)

	if _, err := getSuggestedTags(testToken, testURL); err != nil {
		t.Fatalf("getSuggestedTags() error = %v, want nil", err)
	}

	requests := rec.Requests()
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1: %v", len(requests), requests)
	}
	if !strings.Contains(requests[0], "format=json") {
		t.Errorf("request = %q, want it to carry format=json", requests[0])
	}
}

//nolint:paralleltest // writes to the stats singleton
func TestGetSuggestedTags_CapsTagCount(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusOK,
		`[{"popular":["a","b","c","d","e","f","g","h","i","j","k","l"]}]`)
	useEndpoint(t, server.URL)

	got, err := getSuggestedTags(testToken, testURL)
	if err != nil {
		t.Fatalf("getSuggestedTags() error = %v, want nil", err)
	}

	// maxSuggestedTags suggestions plus the marker: ten tags, as documented.
	tagList := strings.Split(got, ",")
	if len(tagList) != maxSuggestedTags+1 {
		t.Errorf("got %d tags, want %d: %v", len(tagList), maxSuggestedTags+1, tagList)
	}
	if tagList[len(tagList)-1] != autoTagMarker {
		t.Errorf("last tag = %q, want %q", tagList[len(tagList)-1], autoTagMarker)
	}
}

func TestCollectTagsFromSuggestions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		suggestions []tagSuggestion
		want        []string
	}{
		{
			name:        "no suggestions yields no tags",
			suggestions: nil,
			want:        nil,
		},
		{
			name:        "empty element yields no tags",
			suggestions: []tagSuggestion{{}},
			want:        nil,
		},
		{
			name: "deduplicates across elements",
			suggestions: []tagSuggestion{
				{Popular: []string{"go", "go"}},
				{Recommended: []string{"go", "code"}},
			},
			want: []string{"go", "code", autoTagMarker},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := collectTagsFromSuggestions(testCase.suggestions)
			if len(got) != len(testCase.want) {
				t.Fatalf("collectTagsFromSuggestions() = %v, want %v", got, testCase.want)
			}
			for i := range got {
				if got[i] != testCase.want[i] {
					t.Errorf("tag %d = %q, want %q", i, got[i], testCase.want[i])
				}
			}
		})
	}
}
