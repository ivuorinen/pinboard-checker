// apply_functions_test.go covers the phase-two functions that perform the
// destructive Pinboard writes. Every test here asserts on an observable effect
// — the requests issued or the counters moved — because the previous versions
// called the function and asserted nothing, and so passed against any behavior.
package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// okServer answers every Pinboard write with a successful result document.
func okServer(t *testing.T) *recordingServer {
	t.Helper()

	return newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, `{"result_code":"done"}`)
	})
}

// A dry run must issue no requests at all. The previous test for this said in a
// comment that it could not check, and asserted nothing.
//
//nolint:paralleltest // mutates the stats singleton and the verbose flag
func TestApplyUpdateAction_DryRunIssuesNoRequests(t *testing.T) {
	resetStats(t)
	setVerbose(t, true)

	rec := okServer(t)
	useEndpoint(t, rec.URL)

	action := BookmarkAction{
		Original: Bookmark{URL: testURL, Title: "Old", Tags: "old"},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
		NewTitle: testTitle,
		NewTags:  "new,tags",
	}

	applyUpdateAction(action, testToken, true)

	if requests := rec.Requests(); len(requests) != 0 {
		t.Errorf("dry run issued %d requests, want 0: %v", len(requests), requests)
	}
	if stats.UpdatesPerformed != 0 {
		t.Errorf("UpdatesPerformed = %d, want 0 in a dry run", stats.UpdatesPerformed)
	}
}

// posts/add is keyed by URL, so writing to a new URL leaves the old bookmark in
// place. The move must be add-then-delete, in that order.
//
//nolint:paralleltest // mutates the stats singleton and the verbose flag
func TestApplyUpdateAction_URLChangeRemovesTheOldBookmark(t *testing.T) {
	resetStats(t)
	setVerbose(t, false)

	rec := okServer(t)
	useEndpoint(t, rec.URL)

	action := BookmarkAction{
		Original: Bookmark{URL: testURL, Title: "Title", Tags: testTagList, Notes: "notes"},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
	}

	applyUpdateAction(action, testToken, false)

	requests := rec.Requests()
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want an add followed by a delete: %v", len(requests), requests)
	}
	if !strings.HasPrefix(requests[0], "/posts/add") {
		t.Errorf("first request = %q, want /posts/add", requests[0])
	}
	if !strings.HasPrefix(requests[1], "/posts/delete") {
		t.Errorf("second request = %q, want /posts/delete", requests[1])
	}
	// The delete must target the stale URL, never the new one.
	if !strings.Contains(requests[1], "url=http%3A%2F%2Fexample.com") {
		t.Errorf("delete request = %q, want it to target the original URL", requests[1])
	}
	if stats.UpdatesPerformed != 1 {
		t.Errorf("UpdatesPerformed = %d, want 1", stats.UpdatesPerformed)
	}
}

//nolint:paralleltest // mutates the stats singleton and the verbose flag
func TestApplyUpdateAction_TitleOnlyDoesNotDelete(t *testing.T) {
	resetStats(t)
	setVerbose(t, false)

	rec := okServer(t)
	useEndpoint(t, rec.URL)

	action := BookmarkAction{
		Original: Bookmark{URL: testURL, Title: "", Tags: testTagList},
		Action:   ActionUpdate,
		NewTitle: testTitle,
	}

	applyUpdateAction(action, testToken, false)

	requests := rec.Requests()
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want only an add: %v", len(requests), requests)
	}
	if !strings.HasPrefix(requests[0], "/posts/add") {
		t.Errorf("request = %q, want /posts/add", requests[0])
	}
}

// A rejected write must be counted even without -verbose. Gating the record on
// the flag meant a default run reported only successes and exited 0.
//
//nolint:paralleltest // mutates the stats singleton and the verbose flag
func TestApplyUpdateAction_CountsFailureWhenNotVerbose(t *testing.T) {
	resetStats(t)
	setVerbose(t, false)

	server := staticServer(t, http.StatusOK, `{"result_code":"something went wrong"}`)
	useEndpoint(t, server.URL)

	action := BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionUpdate,
		NewTitle: testTitle,
	}

	applyUpdateAction(action, testToken, false)

	if stats.ApplyErrors != 1 {
		t.Errorf("ApplyErrors = %d, want 1", stats.ApplyErrors)
	}
	if stats.UpdatesPerformed != 0 {
		t.Errorf("UpdatesPerformed = %d, want 0 for a rejected write", stats.UpdatesPerformed)
	}
	if got := stats.ApplyErrorCount(); got != 1 {
		t.Errorf("ApplyErrorCount() = %d, want 1", got)
	}
}

//nolint:paralleltest // mutates the stats singleton and the verbose flag
func TestApplyDeleteAction_DryRunIssuesNoRequests(t *testing.T) {
	resetStats(t)
	setVerbose(t, true)

	rec := okServer(t)
	useEndpoint(t, rec.URL)

	action := BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionDelete,
		Error:    errors.New("404 not found"),
	}

	applyDeleteAction(action, testToken, true)

	if requests := rec.Requests(); len(requests) != 0 {
		t.Errorf("dry run issued %d requests, want 0: %v", len(requests), requests)
	}
	if stats.DeletionsPerformed != 0 {
		t.Errorf("DeletionsPerformed = %d, want 0 in a dry run", stats.DeletionsPerformed)
	}
}

//nolint:paralleltest // mutates the stats singleton and the verbose flag
func TestApplyDeleteAction_Success(t *testing.T) {
	resetStats(t)
	setVerbose(t, true)

	rec := okServer(t)
	useEndpoint(t, rec.URL)

	action := BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionDelete,
		Error:    errors.New("404 not found"),
	}

	applyDeleteAction(action, testToken, false)

	requests := rec.Requests()
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1: %v", len(requests), requests)
	}
	if !strings.HasPrefix(requests[0], "/posts/delete") {
		t.Errorf("request = %q, want /posts/delete", requests[0])
	}
	if stats.DeletionsPerformed != 1 {
		t.Errorf("DeletionsPerformed = %d, want 1", stats.DeletionsPerformed)
	}
	if stats.ApplyErrors != 0 {
		t.Errorf("ApplyErrors = %d, want 0", stats.ApplyErrors)
	}
}

//nolint:paralleltest // mutates the stats singleton and the verbose flag
func TestApplyDeleteAction_CountsFailure(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "server error", status: http.StatusInternalServerError, body: ""},
		// Pinboard answers an unknown URL with 200 and this result code.
		{name: "item not found", status: http.StatusOK, body: `{"result_code":"item not found"}`},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resetStats(t)
			setVerbose(t, false)

			server := staticServer(t, testCase.status, testCase.body)
			useEndpoint(t, server.URL)

			action := BookmarkAction{
				Original: Bookmark{URL: testURL},
				Action:   ActionDelete,
				Error:    errors.New("404 not found"),
			}

			applyDeleteAction(action, testToken, false)

			if stats.ApplyErrors != 1 {
				t.Errorf("ApplyErrors = %d, want 1", stats.ApplyErrors)
			}
			if stats.DeletionsPerformed != 0 {
				t.Errorf("DeletionsPerformed = %d, want 0", stats.DeletionsPerformed)
			}
		})
	}
}

//nolint:paralleltest // mutates the stats singleton and the mode flags
func TestApplyBookmarkActions_NoActionsIssuesNoRequests(t *testing.T) {
	resetStats(t)
	setVerbose(t, true)

	rec := okServer(t)
	useEndpoint(t, rec.URL)

	setCIMode(t, false)

	applyBookmarkActions([]BookmarkAction{
		{Action: ActionNoChange},
		{Action: ActionNoChange},
	}, testToken, false)

	if requests := rec.Requests(); len(requests) != 0 {
		t.Errorf("issued %d requests for no-change actions, want 0: %v", len(requests), requests)
	}
}

//nolint:paralleltest // mutates the stats singleton and the mode flags
func TestApplyBookmarkActions_AppliesUpdatesAndDeletes(t *testing.T) {
	resetStats(t)
	setVerbose(t, false)

	rec := okServer(t)
	useEndpoint(t, rec.URL)

	setCIMode(t, false)

	applyBookmarkActions([]BookmarkAction{
		{
			Original: Bookmark{URL: testURL, Title: testTitleAlt},
			Action:   ActionUpdate,
			NewTitle: testTitle,
		},
		{
			Original: Bookmark{URL: "http://dead.example.com"},
			Action:   ActionDelete,
			Error:    errors.New("404 not found"),
		},
		{
			Original: Bookmark{URL: "http://good.example.com"},
			Action:   ActionNoChange,
		},
	}, testToken, false)

	// One add for the update, one delete for the dead link, nothing for the
	// unchanged bookmark.
	requests := rec.Requests()
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2: %v", len(requests), requests)
	}
	if stats.UpdatesPerformed != 1 {
		t.Errorf("UpdatesPerformed = %d, want 1", stats.UpdatesPerformed)
	}
	if stats.DeletionsPerformed != 1 {
		t.Errorf("DeletionsPerformed = %d, want 1", stats.DeletionsPerformed)
	}
}

//nolint:paralleltest // mutates the stats singleton and the mode flags
func TestApplyBookmarkActions_CIModeStillApplies(t *testing.T) {
	resetStats(t)
	setVerbose(t, false)

	rec := okServer(t)
	useEndpoint(t, rec.URL)

	setCIMode(t, true)

	applyBookmarkActions([]BookmarkAction{
		{
			Original: Bookmark{URL: testURL},
			Action:   ActionUpdate,
			NewTitle: testTitle,
		},
	}, testToken, false)

	// Suppressing the progress bar must not suppress the work.
	if requests := rec.Requests(); len(requests) != 1 {
		t.Errorf("got %d requests in CI mode, want 1: %v", len(requests), requests)
	}
	if stats.UpdatesPerformed != 1 {
		t.Errorf("UpdatesPerformed = %d, want 1", stats.UpdatesPerformed)
	}
}
