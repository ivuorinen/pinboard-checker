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

// A 403 that the GET retry confirms is a genuine dead link.
//
//nolint:paralleltest // writes to the stats singleton
func TestCheckURL_ConfirmedForbiddenIsDead(t *testing.T) {
	resetStats(t)

	server := staticServer(t, http.StatusForbidden, "")

	_, err := checkURL(server.URL, "test context")
	if !errors.Is(err, errDeadLink) {
		t.Errorf("checkURL() error = %v, want errDeadLink when GET also refuses", err)
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

	// Pinboard suggesting nothing is a success with no tags, not a failure, and
	// must not make the bookmark look changed.
	t.Run("tags with no suggestions", func(t *testing.T) {
		resetStats(t)
		setSkipFlags(t, true, false)

		server := staticServer(t, http.StatusOK, `[]`)
		useEndpoint(t, server.URL)

		if got := fetchTagsIfNeeded(Bookmark{URL: testURL}, testToken, ""); got != "" {
			t.Errorf("fetchTagsIfNeeded() = %q, want empty when nothing is suggested", got)
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
