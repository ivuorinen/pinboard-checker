package main

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// errTestFetch stands in for a transport failure, so the counter tests can tell
// a failed fetch apart from one that succeeded with nothing to add.
var errTestFetch = errors.New("fetch failed")

func TestRecordAction_NoChange(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	action := BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionNoChange,
	}

	stats.RecordAction(action)

	if stats.ActionNoChange != 1 {
		t.Errorf("expected ActionNoChange to be 1, got %d", stats.ActionNoChange)
	}
	if stats.ActionUpdate != 0 || stats.ActionDelete != 0 {
		t.Error("unexpected action counts")
	}
}

func TestRecordAction_Update(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	action := BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionUpdate,
		NewURL:   "http://example.org",
		NewTitle: "New Title",
		NewTags:  "tag1,tag2",
	}

	stats.RecordAction(action)

	if stats.ActionUpdate != 1 {
		t.Errorf("expected ActionUpdate to be 1, got %d", stats.ActionUpdate)
	}
	if stats.URLsChanged != 1 {
		t.Errorf("expected URLsChanged to be 1, got %d", stats.URLsChanged)
	}
	if stats.TitlesAdded != 1 {
		t.Errorf("expected TitlesAdded to be 1, got %d", stats.TitlesAdded)
	}
	if stats.TagsAdded != 1 {
		t.Errorf("expected TagsAdded to be 1, got %d", stats.TagsAdded)
	}
}

func TestRecordAction_UpdateSameURL(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	action := BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionUpdate,
		NewURL:   "http://example.com", // Same URL
		NewTitle: "New Title",
	}

	stats.RecordAction(action)

	if stats.URLsChanged != 0 {
		t.Errorf("expected URLsChanged to be 0 when URL unchanged, got %d", stats.URLsChanged)
	}
	if stats.TitlesAdded != 1 {
		t.Errorf("expected TitlesAdded to be 1, got %d", stats.TitlesAdded)
	}
}

func TestRecordAction_Delete(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	action := BookmarkAction{
		Original: Bookmark{URL: testURL},
		Action:   ActionDelete,
		Error:    errors.New("404 not found"),
	}

	stats.RecordAction(action)

	if stats.ActionDelete != 1 {
		t.Errorf("expected ActionDelete to be 1, got %d", stats.ActionDelete)
	}
	if stats.ActionUpdate != 0 || stats.ActionNoChange != 0 {
		t.Error("a delete must not increment the other action counters")
	}
}

func TestRecordHTTPStatus(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	stats.RecordHTTPStatus("url-validation", 200)
	stats.RecordHTTPStatus("url-validation", 200)
	stats.RecordHTTPStatus("url-validation", 404)
	stats.RecordHTTPStatus("title-fetch", 200)

	if stats.StatusCodes["url-validation"][200] != 2 {
		t.Errorf("expected 2 url-validation 200s, got %d", stats.StatusCodes["url-validation"][200])
	}
	if stats.StatusCodes["url-validation"][404] != 1 {
		t.Errorf("expected 1 url-validation 404, got %d", stats.StatusCodes["url-validation"][404])
	}
	if stats.StatusCodes["title-fetch"][200] != 1 {
		t.Errorf("expected 1 title-fetch 200, got %d", stats.StatusCodes["title-fetch"][200])
	}
}

func TestIncrementParenthesesFixed(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	stats.IncrementParenthesesFixed()
	stats.IncrementParenthesesFixed()

	if stats.ParenthesesFixed != 2 {
		t.Errorf("expected ParenthesesFixed to be 2, got %d", stats.ParenthesesFixed)
	}
}

func TestIncrementRedirects(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	stats.IncrementRedirects()
	stats.IncrementRedirects()
	stats.IncrementRedirects()

	if stats.Redirects != 3 {
		t.Errorf("expected Redirects to be 3, got %d", stats.Redirects)
	}
}

func TestRecordTitleFetch(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	stats.RecordTitleFetch(true, nil)
	stats.RecordTitleFetch(true, nil)
	stats.RecordTitleFetch(false, errTestFetch)
	// A page with no <title>: the fetch worked, there was nothing to take.
	stats.RecordTitleFetch(false, nil)

	if stats.TitlesFetched != 2 {
		t.Errorf("expected TitlesFetched to be 2, got %d", stats.TitlesFetched)
	}
	if stats.TitlesFailed != 1 {
		t.Errorf("expected TitlesFailed to be 1, got %d", stats.TitlesFailed)
	}
	if stats.TitlesEmpty != 1 {
		t.Errorf("expected TitlesEmpty to be 1, got %d", stats.TitlesEmpty)
	}
}

func TestRecordTagFetch(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	stats.RecordTagFetch(true, nil)
	stats.RecordTagFetch(false, errTestFetch)
	stats.RecordTagFetch(false, errTestFetch)
	// Pinboard had no suggestions: a successful call with nothing to add, which
	// is the common case for an untagged bookmark and never a failure.
	stats.RecordTagFetch(false, nil)

	if stats.TagsFetched != 1 {
		t.Errorf("expected TagsFetched to be 1, got %d", stats.TagsFetched)
	}
	if stats.TagsFailed != 2 {
		t.Errorf("expected TagsFailed to be 2, got %d", stats.TagsFailed)
	}
	if stats.TagsEmpty != 1 {
		t.Errorf("expected TagsEmpty to be 1, got %d", stats.TagsEmpty)
	}
}

func TestIncrementUpdates(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	stats.IncrementUpdates()
	stats.IncrementUpdates()

	if stats.UpdatesPerformed != 2 {
		t.Errorf("expected UpdatesPerformed to be 2, got %d", stats.UpdatesPerformed)
	}
}

func TestIncrementDeletions(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	stats.IncrementDeletions()

	if stats.DeletionsPerformed != 1 {
		t.Errorf("expected DeletionsPerformed to be 1, got %d", stats.DeletionsPerformed)
	}
}

func TestThreadSafety(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	var wg sync.WaitGroup
	operations := 100

	// Concurrently increment various counters
	for i := 0; i < operations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats.IncrementParenthesesFixed()
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			stats.IncrementRedirects()
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			stats.RecordTitleFetch(true, nil)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			stats.RecordHTTPStatus("test", 200)
		}()
	}

	wg.Wait()

	if stats.ParenthesesFixed != operations {
		t.Errorf("expected ParenthesesFixed to be %d, got %d", operations, stats.ParenthesesFixed)
	}
	if stats.Redirects != operations {
		t.Errorf("expected Redirects to be %d, got %d", operations, stats.Redirects)
	}
	if stats.TitlesFetched != operations {
		t.Errorf("expected TitlesFetched to be %d, got %d", operations, stats.TitlesFetched)
	}
	if stats.StatusCodes["test"][200] != operations {
		t.Errorf("expected 200 status codes to be %d, got %d", operations, stats.StatusCodes["test"][200])
	}
}

func TestPrint_EmptyStatistics(t *testing.T) {
	t.Parallel()

	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	// Capture stdout
	output := render(stats.Print)

	// Should contain the header
	if !strings.Contains(output, "PROCESSING STATISTICS") {
		t.Error("expected output to contain 'PROCESSING STATISTICS'")
	}

	// Should contain timing section
	if !strings.Contains(output, "TIMING") {
		t.Error("expected output to contain 'TIMING'")
	}

	// Should contain actions section
	if !strings.Contains(output, "ACTIONS") {
		t.Error("expected output to contain 'ACTIONS'")
	}
}

func TestPrint_WithData(t *testing.T) {
	t.Parallel()

	stats := &Statistics{
		StatusCodes:        make(map[string]map[int]int),
		TotalBookmarks:     100,
		ActionNoChange:     80,
		ActionUpdate:       15,
		ActionDelete:       5,
		URLsChanged:        10,
		TitlesAdded:        5,
		ParenthesesFixed:   3,
		Redirects:          2,
		TitlesFetched:      5,
		UpdatesPerformed:   15,
		ProcessingDuration: 10 * time.Second,
	}

	stats.RecordHTTPStatus("url-validation", 200)

	output := render(stats.Print)

	// Check for key statistics
	if !strings.Contains(output, "Total bookmarks:    100") {
		t.Error("expected output to contain total bookmarks")
	}
	if !strings.Contains(output, "UPDATES BREAKDOWN") {
		t.Error("expected output to contain updates breakdown")
	}
	if !strings.Contains(output, "URL PROCESSING") {
		t.Error("expected output to contain URL processing")
	}
	if !strings.Contains(output, "HTTP RESPONSES") {
		t.Error("expected output to contain HTTP responses")
	}
	if !strings.Contains(output, "API OPERATIONS") {
		t.Error("expected output to contain API operations")
	}
}

func TestPrint_NoUpdatesBreakdown(t *testing.T) {
	t.Parallel()

	stats := &Statistics{
		StatusCodes:    make(map[string]map[int]int),
		TotalBookmarks: 100,
		ActionNoChange: 100,
	}

	output := render(stats.Print)

	// Should not contain updates breakdown if no updates
	if strings.Contains(output, "UPDATES BREAKDOWN") {
		t.Error("expected output to NOT contain updates breakdown when no updates")
	}
}

func TestPrint_NoURLProcessing(t *testing.T) {
	t.Parallel()

	stats := &Statistics{
		StatusCodes:    make(map[string]map[int]int),
		TotalBookmarks: 100,
	}

	output := render(stats.Print)

	// Should not contain URL processing if none occurred
	if strings.Contains(output, "URL PROCESSING") {
		t.Error("expected output to NOT contain URL processing when none occurred")
	}
}

func TestPrint_NoHTTPResponses(t *testing.T) {
	t.Parallel()

	stats := &Statistics{
		StatusCodes:    make(map[string]map[int]int),
		TotalBookmarks: 100,
	}

	output := render(stats.Print)

	// Should not contain HTTP responses if none recorded
	if strings.Contains(output, "HTTP RESPONSES") {
		t.Error("expected output to NOT contain HTTP responses when none recorded")
	}
}

func TestPrint_NoAPIOperations(t *testing.T) {
	t.Parallel()

	stats := &Statistics{
		StatusCodes:    make(map[string]map[int]int),
		TotalBookmarks: 100,
	}

	output := render(stats.Print)

	// Should not contain API operations if none occurred
	if strings.Contains(output, "API OPERATIONS") {
		t.Error("expected output to NOT contain API operations when none occurred")
	}
}

func TestPrint_HTTPOperationNames(t *testing.T) {
	t.Parallel()

	stats := &Statistics{
		StatusCodes: make(map[string]map[int]int),
	}

	stats.RecordHTTPStatus("url-validation", 200)
	stats.RecordHTTPStatus("title-fetch", 404)
	stats.RecordHTTPStatus("tag-fetch", 200)

	output := render(stats.Print)

	// Check that operation names are formatted correctly
	if !strings.Contains(output, "URL Validation") {
		t.Error("expected 'url-validation' to be formatted as 'URL Validation'")
	}
	if !strings.Contains(output, "Title Fetch") {
		t.Error("expected 'title-fetch' to be formatted as 'Title Fetch'")
	}
	if !strings.Contains(output, "Tag Fetch") {
		t.Error("expected 'tag-fetch' to be formatted as 'Tag Fetch'")
	}
}

// Every conditional line in the API OPERATIONS block must render when its
// counter is set. Each was added for a defect the report had previously hidden,
// so a line that silently stopped printing would take the evidence with it.
func TestPrintAPIOperations_RendersEveryCounter(t *testing.T) {
	t.Parallel()

	report := newStatistics()
	report.TotalBookmarks = 1
	report.TitlesFetched = 3
	report.TitlesEmpty = 7
	report.TitlesFailed = 2
	report.TagsFetched = 1
	report.TagsEmpty = 4
	report.UpdatesPerformed = 5
	report.DeletionsPerformed = 6
	report.Merged = 8
	report.ApplyErrors = 9

	printed := render(report.Print)

	for _, want := range []string{
		"Titles fetched:     3",
		"(7 with nothing to add)",
		"(2 failed)",
		"(4 with nothing to add)",
		"Updates performed:  5",
		"Deletions performed: 6",
		"Duplicates merged:  8",
		"Writes rejected:    9",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("report is missing %q\n---\n%s", want, printed)
		}
	}
}

// render returns what printFunc writes. It replaces a helper that swapped the
// process-wide os.Stdout through a pipe, which kept every report test serial.
func render(printFunc func(io.Writer)) string {
	var buf strings.Builder

	printFunc(&buf)

	return buf.String()
}
