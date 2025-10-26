package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecordAction_NoChange(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com"},
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
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com"},
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
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com"},
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
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com"},
		Action:   ActionDelete,
		Error:    errors.New("404 not found"),
	}

	stats.RecordAction(action)

	if stats.ActionDelete != 1 {
		t.Errorf("expected ActionDelete to be 1, got %d", stats.ActionDelete)
	}
	if stats.TotalErrors != 1 {
		t.Errorf("expected TotalErrors to be 1, got %d", stats.TotalErrors)
	}
}

func TestRecordHTTPStatus(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
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

func TestRecordShortenerExpansion(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	stats.RecordShortenerExpansion("bitly", true)
	stats.RecordShortenerExpansion("bitly", true)
	stats.RecordShortenerExpansion("bitly", false)
	stats.RecordShortenerExpansion("tinyurl", true)

	if stats.ShortURLsExpanded["bitly"] != 2 {
		t.Errorf("expected 2 bitly successes, got %d", stats.ShortURLsExpanded["bitly"])
	}
	if stats.ShortURLsFailed["bitly"] != 1 {
		t.Errorf("expected 1 bitly failure, got %d", stats.ShortURLsFailed["bitly"])
	}
	if stats.ShortURLsExpanded["tinyurl"] != 1 {
		t.Errorf("expected 1 tinyurl success, got %d", stats.ShortURLsExpanded["tinyurl"])
	}
}

func TestIncrementParenthesesFixed(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
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
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
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
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	stats.RecordTitleFetch(true)
	stats.RecordTitleFetch(true)
	stats.RecordTitleFetch(false)

	if stats.TitlesFetched != 2 {
		t.Errorf("expected TitlesFetched to be 2, got %d", stats.TitlesFetched)
	}
	if stats.TitlesFailed != 1 {
		t.Errorf("expected TitlesFailed to be 1, got %d", stats.TitlesFailed)
	}
}

func TestRecordTagFetch(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	stats.RecordTagFetch(true)
	stats.RecordTagFetch(false)
	stats.RecordTagFetch(false)

	if stats.TagsFetched != 1 {
		t.Errorf("expected TagsFetched to be 1, got %d", stats.TagsFetched)
	}
	if stats.TagsFailed != 2 {
		t.Errorf("expected TagsFailed to be 2, got %d", stats.TagsFailed)
	}
}

func TestIncrementUpdates(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
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
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	stats.IncrementDeletions()

	if stats.DeletionsPerformed != 1 {
		t.Errorf("expected DeletionsPerformed to be 1, got %d", stats.DeletionsPerformed)
	}
}

func TestThreadSafety(t *testing.T) {
	t.Parallel()
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
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
			stats.RecordTitleFetch(true)
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

//nolint:paralleltest // Cannot run in parallel - modifies global os.Stdout
func TestPrint_EmptyStatistics(t *testing.T) {
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	// Capture stdout
	output := captureOutput(func() {
		stats.Print()
	})

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

//nolint:paralleltest // Cannot run in parallel - modifies global os.Stdout
func TestPrint_WithData(t *testing.T) {
	stats := &Statistics{
		ShortURLsExpanded:  make(map[string]int),
		ShortURLsFailed:    make(map[string]int),
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

	stats.ShortURLsExpanded["bitly"] = 2
	stats.RecordHTTPStatus("url-validation", 200)

	output := captureOutput(func() {
		stats.Print()
	})

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
	if !strings.Contains(output, "bit.ly") {
		t.Error("expected output to contain shortener stats")
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies global os.Stdout
func TestPrint_NoUpdatesBreakdown(t *testing.T) {
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
		TotalBookmarks:    100,
		ActionNoChange:    100,
	}

	output := captureOutput(func() {
		stats.Print()
	})

	// Should not contain updates breakdown if no updates
	if strings.Contains(output, "UPDATES BREAKDOWN") {
		t.Error("expected output to NOT contain updates breakdown when no updates")
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies global os.Stdout
func TestPrint_NoURLProcessing(t *testing.T) {
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
		TotalBookmarks:    100,
	}

	output := captureOutput(func() {
		stats.Print()
	})

	// Should not contain URL processing if none occurred
	if strings.Contains(output, "URL PROCESSING") {
		t.Error("expected output to NOT contain URL processing when none occurred")
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies global os.Stdout
func TestPrint_NoHTTPResponses(t *testing.T) {
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
		TotalBookmarks:    100,
	}

	output := captureOutput(func() {
		stats.Print()
	})

	// Should not contain HTTP responses if none recorded
	if strings.Contains(output, "HTTP RESPONSES") {
		t.Error("expected output to NOT contain HTTP responses when none recorded")
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies global os.Stdout
func TestPrint_NoAPIOperations(t *testing.T) {
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
		TotalBookmarks:    100,
	}

	output := captureOutput(func() {
		stats.Print()
	})

	// Should not contain API operations if none occurred
	if strings.Contains(output, "API OPERATIONS") {
		t.Error("expected output to NOT contain API operations when none occurred")
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies global os.Stdout
func TestPrint_HTTPOperationNames(t *testing.T) {
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	stats.RecordHTTPStatus("url-validation", 200)
	stats.RecordHTTPStatus("title-fetch", 404)
	stats.RecordHTTPStatus("tag-fetch", 200)

	output := captureOutput(func() {
		stats.Print()
	})

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

//nolint:paralleltest // Cannot run in parallel - modifies global os.Stdout
func TestPrint_AllShorteners(t *testing.T) {
	stats := &Statistics{
		ShortURLsExpanded: make(map[string]int),
		ShortURLsFailed:   make(map[string]int),
		StatusCodes:       make(map[string]map[int]int),
	}

	stats.ShortURLsExpanded["bitly"] = 5
	stats.ShortURLsFailed["bitly"] = 1
	stats.ShortURLsExpanded["tinyurl"] = 3
	stats.ShortURLsExpanded["isgd"] = 2
	stats.ShortURLsFailed["isgd"] = 1

	output := captureOutput(func() {
		stats.Print()
	})

	// Check that all shorteners are displayed
	if !strings.Contains(output, "bit.ly") {
		t.Error("expected output to contain bit.ly stats")
	}
	if !strings.Contains(output, "tinyurl") {
		t.Error("expected output to contain tinyurl stats")
	}
	if !strings.Contains(output, "is.gd") {
		t.Error("expected output to contain is.gd stats")
	}
}

// captureOutput captures stdout during function execution.
func captureOutput(printFunc func()) string {
	old := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	os.Stdout = write

	// Channel to hold the output
	outChan := make(chan string)

	// Copy the output in a separate goroutine
	go func() {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, read); err != nil {
			panic(err)
		}
		outChan <- buf.String()
	}()

	printFunc()

	if err := write.Close(); err != nil {
		panic(err)
	}
	os.Stdout = old

	return <-outChan
}
