// apply_functions_test.go covers the phase-two functions that perform the
// destructive Pinboard writes. Every test here asserts on an observable effect
// — the requests issued or the counters moved — because the previous versions
// called the function and asserted nothing, and so passed against any behavior.
package main

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// okServer answers every Pinboard write with a successful result document.
func okServer(t *testing.T) *recordingServer {
	t.Helper()

	return newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, testDoneBody)
	})
}

// newWriteChecker returns a test checker whose Pinboard endpoint is server.
func newWriteChecker(t *testing.T, server string) *checker {
	t.Helper()

	chk := newTestChecker(t)
	chk.pinboardEndpoint = server

	return chk
}

// A dry run must issue no requests at all. The previous test for this said in a
// comment that it could not check, and asserted nothing.
func TestApplyUpdateAction_DryRunIssuesNoRequests(t *testing.T) {
	t.Parallel()

	rec := okServer(t)
	chk := newWriteChecker(t, rec.URL)
	chk.verbose = true
	chk.dryRun = true

	chk.applyUpdateAction(BookmarkAction{
		Original: Bookmark{URL: testURL, Title: "Old", Tags: "old"},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
		NewTitle: testTitle,
		NewTags:  "new,tags",
	})

	if requests := rec.Requests(); len(requests) != 0 {
		t.Errorf("dry run issued %d requests, want 0: %v", len(requests), requests)
	}
	if chk.stats.UpdatesPerformed != 0 {
		t.Errorf("UpdatesPerformed = %d, want 0 in a dry run", chk.stats.UpdatesPerformed)
	}
}

// posts/add is keyed by URL, so writing to a new URL leaves the old bookmark in
// place. The move must be add-then-delete, in that order.
func TestApplyUpdateAction_URLChangeRemovesTheOldBookmark(t *testing.T) {
	t.Parallel()

	rec := okServer(t)
	chk := newWriteChecker(t, rec.URL)

	chk.applyUpdateAction(BookmarkAction{
		Original: Bookmark{URL: testURL, Title: "Title", Tags: testTagList, Notes: "notes"},
		Action:   ActionUpdate,
		NewURL:   testNewURL,
	})

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
	if chk.stats.UpdatesPerformed != 1 {
		t.Errorf("UpdatesPerformed = %d, want 1", chk.stats.UpdatesPerformed)
	}
}

func TestApplyUpdateAction_TitleOnlyDoesNotDelete(t *testing.T) {
	t.Parallel()

	rec := okServer(t)
	chk := newWriteChecker(t, rec.URL)

	chk.applyUpdateAction(BookmarkAction{
		Original: Bookmark{URL: testURL, Title: "", Tags: testTagList},
		Action:   ActionUpdate,
		NewTitle: testTitle,
	})

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
func TestApplyUpdateAction_CountsFailureWhenNotVerbose(t *testing.T) {
	t.Parallel()

	chk := newWriteChecker(t,
		staticServer(t, http.StatusOK, `{"result_code":"something went wrong"}`).URL)

	chk.applyUpdateAction(BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionUpdate,
		NewTitle: testTitle,
	})

	if chk.stats.ApplyErrors != 1 {
		t.Errorf("ApplyErrors = %d, want 1", chk.stats.ApplyErrors)
	}
	if chk.stats.UpdatesPerformed != 0 {
		t.Errorf("UpdatesPerformed = %d, want 0 for a rejected write", chk.stats.UpdatesPerformed)
	}
	if got := chk.stats.ApplyErrorCount(); got != 1 {
		t.Errorf("ApplyErrorCount() = %d, want 1", got)
	}
}

// Two bookmarks moving to one address the account did not hold: the first is
// written, the second is merged into it. Both were written with replace=yes,
// so the second overwrote the first and the report showed two updates and no
// merge.
func TestApplyBookmarkActions_ConvergingMoversMerge(t *testing.T) {
	t.Parallel()

	rec := okServer(t)
	chk := newWriteChecker(t, rec.URL)
	chk.ciMode = true

	target := "https://example.org/article"
	chk.applyBookmarkActions([]BookmarkAction{
		{Original: Bookmark{URL: "https://bit.ly/a", Notes: "notes-A"},
			Action: ActionUpdate, NewURL: target},
		{Original: Bookmark{URL: "https://tinyurl.com/b", Notes: "notes-B"},
			Action: ActionUpdate, NewURL: target},
	})

	adds := 0
	for _, request := range rec.Requests() {
		if strings.HasPrefix(request, "/posts/add") {
			adds++
			if !strings.Contains(request, "notes-A") {
				t.Errorf("add = %q, want the first mover's fields", request)
			}
		}
	}
	if adds != 1 {
		t.Errorf("got %d posts/add, want 1: %v", adds, rec.Requests())
	}
	if chk.stats.Merged != 1 || chk.stats.UpdatesPerformed != 1 {
		t.Errorf("Merged = %d, UpdatesPerformed = %d; want 1 and 1",
			chk.stats.Merged, chk.stats.UpdatesPerformed)
	}
}

// narratedUpdate is the prefix of a real, applied update's narration.
const narratedUpdate = "Updated: "

// -verbose narrates what an update did, not what it planned: a rejected write
// is no update, a merge deletes the bookmark, and a dry run says "Would".
func TestApplyUpdateAction_NarratesTheOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		dryRun  bool
		known   bool
		want    string
		notWant string
	}{
		{name: "update", body: testDoneBody, want: narratedUpdate},
		{name: "rejected write", body: `{"result_code":"something went wrong"}`,
			notWant: narratedUpdate},
		{name: "merge", body: testDoneBody, known: true,
			want: "Merged: ", notWant: narratedUpdate},
		{name: "dry-run update", dryRun: true, want: "Would update: ", notWant: narratedUpdate},
		{name: "dry-run merge", dryRun: true, known: true,
			want: "Would merge: ", notWant: "update: "},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			chk := newWriteChecker(t, staticServer(t, http.StatusOK, testCase.body).URL)
			chk.verbose = true
			chk.dryRun = testCase.dryRun
			chk.knownURLs[testNewURL] = testCase.known

			chk.applyUpdateAction(BookmarkAction{
				Original: Bookmark{URL: testURL},
				Action:   ActionUpdate,
				NewURL:   testNewURL,
			})

			output := stdoutOf(chk)
			if testCase.want != "" && !strings.Contains(output, testCase.want) {
				t.Errorf("stdout = %q, want %q", output, testCase.want)
			}
			if testCase.notWant != "" && strings.Contains(output, testCase.notWant) {
				t.Errorf("stdout = %q, must not contain %q", output, testCase.notWant)
			}
		})
	}
}

// A dry run previews converging movers the way the real run applies them: the
// second one as a merge.
func TestApplyBookmarkActions_DryRunPreviewsConvergingMoverAsMerge(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.verbose = true
	chk.dryRun = true
	chk.ciMode = true

	chk.applyBookmarkActions([]BookmarkAction{
		{Original: Bookmark{URL: "https://bit.ly/a"}, Action: ActionUpdate, NewURL: testNewURL},
		{Original: Bookmark{URL: "https://bit.ly/b"}, Action: ActionUpdate, NewURL: testNewURL},
	})

	output := stdoutOf(chk)
	if !strings.Contains(output, "Would update: https://bit.ly/a") ||
		!strings.Contains(output, "Would merge: https://bit.ly/b") {
		t.Errorf("stdout = %q, want an update then a merge", output)
	}
}

func TestApplyDeleteAction_DryRunIssuesNoRequests(t *testing.T) {
	t.Parallel()

	rec := okServer(t)
	chk := newWriteChecker(t, rec.URL)
	chk.verbose = true
	chk.dryRun = true

	chk.applyDeleteAction(BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionDelete,
		Error:    errors.New("404 not found"),
	})

	if requests := rec.Requests(); len(requests) != 0 {
		t.Errorf("dry run issued %d requests, want 0: %v", len(requests), requests)
	}
	if chk.stats.DeletionsPerformed != 0 {
		t.Errorf("DeletionsPerformed = %d, want 0 in a dry run", chk.stats.DeletionsPerformed)
	}
}

func TestApplyDeleteAction_Success(t *testing.T) {
	t.Parallel()

	rec := okServer(t)
	chk := newWriteChecker(t, rec.URL)
	chk.verbose = true

	chk.applyDeleteAction(BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionDelete,
		Error:    errors.New("404 not found"),
	})

	requests := rec.Requests()
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1: %v", len(requests), requests)
	}
	if !strings.HasPrefix(requests[0], "/posts/delete") {
		t.Errorf("request = %q, want /posts/delete", requests[0])
	}
	if chk.stats.DeletionsPerformed != 1 {
		t.Errorf("DeletionsPerformed = %d, want 1", chk.stats.DeletionsPerformed)
	}
	if chk.stats.ApplyErrors != 0 {
		t.Errorf("ApplyErrors = %d, want 0", chk.stats.ApplyErrors)
	}
}

func TestApplyDeleteAction_CountsFailure(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			chk := newWriteChecker(t, staticServer(t, testCase.status, testCase.body).URL)

			chk.applyDeleteAction(BookmarkAction{
				Original: Bookmark{URL: testURL},
				Action:   ActionDelete,
				Error:    errors.New("404 not found"),
			})

			if chk.stats.ApplyErrors != 1 {
				t.Errorf("ApplyErrors = %d, want 1", chk.stats.ApplyErrors)
			}
			if chk.stats.DeletionsPerformed != 0 {
				t.Errorf("DeletionsPerformed = %d, want 0", chk.stats.DeletionsPerformed)
			}
		})
	}
}

func TestApplyBookmarkActions_NoActionsIssuesNoRequests(t *testing.T) {
	t.Parallel()

	rec := okServer(t)
	chk := newWriteChecker(t, rec.URL)
	chk.verbose = true

	chk.applyBookmarkActions([]BookmarkAction{
		{Action: ActionNoChange},
		{Action: ActionNoChange},
	})

	if requests := rec.Requests(); len(requests) != 0 {
		t.Errorf("issued %d requests for no-change actions, want 0: %v", len(requests), requests)
	}
	if !strings.Contains(stdoutOf(chk), "No changes to apply") {
		t.Errorf("stdout = %q, want the no-changes notice", stdoutOf(chk))
	}
}

func TestApplyBookmarkActions_AppliesUpdatesAndDeletes(t *testing.T) {
	t.Parallel()

	rec := okServer(t)
	chk := newWriteChecker(t, rec.URL)

	chk.applyBookmarkActions([]BookmarkAction{
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
	})

	// One add for the update, one delete for the dead link, nothing for the
	// unchanged bookmark.
	requests := rec.Requests()
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2: %v", len(requests), requests)
	}
	if chk.stats.UpdatesPerformed != 1 {
		t.Errorf("UpdatesPerformed = %d, want 1", chk.stats.UpdatesPerformed)
	}
	if chk.stats.DeletionsPerformed != 1 {
		t.Errorf("DeletionsPerformed = %d, want 1", chk.stats.DeletionsPerformed)
	}
}

func TestApplyBookmarkActions_CIModeStillApplies(t *testing.T) {
	t.Parallel()

	rec := okServer(t)
	chk := newWriteChecker(t, rec.URL)
	chk.ciMode = true

	chk.applyBookmarkActions([]BookmarkAction{
		{
			Original: Bookmark{URL: testURL},
			Action:   ActionUpdate,
			NewTitle: testTitle,
		},
	})

	// Suppressing the progress bar must not suppress the work.
	if requests := rec.Requests(); len(requests) != 1 {
		t.Errorf("got %d requests in CI mode, want 1: %v", len(requests), requests)
	}
	if chk.stats.UpdatesPerformed != 1 {
		t.Errorf("UpdatesPerformed = %d, want 1", chk.stats.UpdatesPerformed)
	}
}

// Every deletion leaves a restorable record on stderr without -verbose:
// Pinboard has no trash, and a run that deleted silently could be neither
// noticed nor undone. Each case is one of the three paths that issue
// posts/delete.
func TestDeletionsLeaveARestorableRecord(t *testing.T) {
	t.Parallel()

	original := Bookmark{
		URL: testURL, Title: testTitle, Notes: "my notes", Tags: "go web",
		Time: testTime, Shared: "no", ToRead: valueYes,
	}

	tests := []struct {
		name   string
		action BookmarkAction
		known  bool
		reason string
	}{
		{
			name:   "dead link",
			action: BookmarkAction{Original: original, Action: ActionDelete},
			reason: "deleted (dead link): ",
		},
		{
			name:   "merge",
			action: BookmarkAction{Original: original, Action: ActionUpdate, NewURL: testNewURL},
			known:  true,
			reason: "deleted (merged into " + testNewURL + "): ",
		},
		{
			name:   "move",
			action: BookmarkAction{Original: original, Action: ActionUpdate, NewURL: testNewURL},
			reason: "deleted (moved to " + testNewURL + "): ",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			chk := newWriteChecker(t, okServer(t).URL)
			chk.ciMode = true
			chk.knownURLs[testNewURL] = testCase.known

			chk.applyBookmarkActions([]BookmarkAction{testCase.action})

			output := stderrOf(chk)
			line, found := strings.CutPrefix(strings.TrimSpace(output), testCase.reason)
			if !found {
				t.Fatalf("stderr = %q, want a line starting %q", output, testCase.reason)
			}
			for _, field := range []string{
				"url=" + url.QueryEscape(testURL), "extended=my+notes", "tags=go%2Cweb",
				"dt=2020-01-02T03%3A04%3A05Z", "shared=no", "toread=yes",
			} {
				if !strings.Contains(line, field) {
					t.Errorf("record %q lacks %q; it must restore the whole bookmark",
						line, field)
				}
			}
		})
	}
}

// A failed delete records nothing: the bookmark is still there, and a record
// would claim otherwise.
func TestFailedDeletionLeavesNoRecord(t *testing.T) {
	t.Parallel()

	chk := newWriteChecker(t,
		staticServer(t, http.StatusOK, `{"result_code":"item not found"}`).URL)
	chk.ciMode = true

	chk.applyBookmarkActions([]BookmarkAction{
		{Original: Bookmark{URL: testURL}, Action: ActionDelete},
	})

	if output := stderrOf(chk); strings.Contains(output, "deleted (") {
		t.Errorf("stderr = %q, want no deletion record for a rejected delete", output)
	}
}
