// main_test.go holds the suite's shared fixtures and helpers alongside the
// tests for title extraction and tag suggestion.
package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
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
	testTime     = "2020-01-02T03:04:05Z"
	// testDoneBody is Pinboard's answer to an accepted write.
	testDoneBody = `{"result_code":"done"}`
	// testBadURL fails url.Parse: the IPv6 literal is never closed.
	testBadURL = "http://[::1"
)

// lockedBuffer is an output stream a test can read back. It is locked because
// the worker pool writes -verbose narration from many goroutines at once, and a
// bare bytes.Buffer under those writes is a data race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// newTestChecker returns a checker of the test's own, so the test can run in
// parallel with every other: the run state that tests used to swap in and out
// of package globals is all on the checker now.
//
// The pacing is shortened and the private-address guard bypassed. The
// production intervals are three seconds per Pinboard call and half a second
// per bookmarked host, which would dominate the run time, and httptest serves
// on 127.0.0.1, which the real guard correctly rejects. The pacing and the
// guard are tested directly, on checkers that restore them.
func newTestChecker(t *testing.T) *checker {
	t.Helper()

	chk := newChecker(&lockedBuffer{}, &lockedBuffer{})
	chk.apiToken = testToken
	chk.limiter = newRateLimiter(time.Millisecond)
	chk.hosts = newHostThrottle(0)
	chk.isPublicHost = func(string) bool { return true }

	return chk
}

// stdoutOf returns everything the checker wrote to its report stream.
func stdoutOf(chk *checker) string {
	return bufferText(chk.out)
}

// stderrOf returns everything the checker wrote to its diagnostic stream.
func stderrOf(chk *checker) string {
	return bufferText(chk.errOut)
}

// bufferText reads back a stream newTestChecker installed. A stream of any
// other type yields "", which fails every assertion that expects output.
func bufferText(stream io.Writer) string {
	buf, ok := stream.(*lockedBuffer)
	if !ok {
		return ""
	}

	return buf.String()
}

// swap sets *target to value for the duration of the test and restores the
// previous value afterwards. Only the process-level seams use it now — osExit,
// readBuildInfo, version — so every test that calls it stays serial.
func swap[T any](t *testing.T, target *T, value T) {
	t.Helper()

	previous := *target
	*target = value

	t.Cleanup(func() { *target = previous })
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

func TestGetPageTitle(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

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

			got, err := newTestChecker(t).getPageTitle(server.URL)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("getPageTitle() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if got != testCase.want {
				t.Errorf("getPageTitle() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestGetPageTitle_TruncatesToPinboardLimit(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("ä", maxPinboardTitleLen+50)
	server := staticServer(t, http.StatusOK, "<html><head><title>"+long+"</title></head></html>")

	got, err := newTestChecker(t).getPageTitle(server.URL)
	if err != nil {
		t.Fatalf("getPageTitle() error = %v, want nil", err)
	}
	// Counted in runes: cutting on bytes would split the multi-byte character.
	if runes := []rune(got); len(runes) != maxPinboardTitleLen {
		t.Errorf("title length = %d runes, want %d", len(runes), maxPinboardTitleLen)
	}
}

// A redirect during the title fetch leads to a page the bookmark does not
// point at; a login wall answers an anonymous GET with one. Following it
// titled the bookmark "Sign in to …", so no title is taken and the target is
// never requested.
func TestGetPageTitle_TakesNoTitleThroughARedirect(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			loginRequests := 0
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/login" {
						loginRequests++
						writeBody(t, w, "<html><head><title>Sign in to Example</title></head></html>")

						return
					}
					http.Redirect(w, r, "/login", status)
				}))
			t.Cleanup(server.Close)

			title, err := newTestChecker(t).getPageTitle(server.URL + "/private/doc")
			if err == nil {
				t.Errorf("getPageTitle() error = nil, want one for a %d answer", status)
			}
			if title != "" {
				t.Errorf("getPageTitle() = %q, want no title from the redirect target", title)
			}
			if loginRequests != 0 {
				t.Errorf("the redirect target got %d requests, want none", loginRequests)
			}
		})
	}
}

func TestGetSuggestedTags(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			chk := newTestChecker(t)
			chk.pinboardEndpoint = staticServer(t, testCase.status, testCase.body).URL

			got, err := chk.getSuggestedTags(testURL)
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
func TestGetSuggestedTags_RequestsJSON(t *testing.T) {
	t.Parallel()

	rec := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, `[{"popular":["go"]}]`)
	})

	chk := newTestChecker(t)
	chk.pinboardEndpoint = rec.URL

	if _, err := chk.getSuggestedTags(testURL); err != nil {
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

func TestGetSuggestedTags_CapsTagCount(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.pinboardEndpoint = staticServer(t, http.StatusOK,
		`[{"popular":["a","b","c","d","e","f","g","h","i","j","k","l"]}]`).URL

	got, err := chk.getSuggestedTags(testURL)
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
