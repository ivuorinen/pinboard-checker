// report_test.go asserts what the statistics block actually renders. The report
// is the tool's only output, so every conditional line in it is checked against
// the counters that produce it rather than merely executed.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// A fully populated report must render every conditional line. Each of these
// counters was added because a run could otherwise finish without saying what
// it had done.
func TestPrint_RendersEveryConditionalLine(t *testing.T) {
	t.Parallel()

	report := newStatistics()
	report.TotalBookmarks = 10
	report.ActionNoChange = 4
	report.ActionUpdate = 3
	report.ActionDelete = 1
	report.Skipped = 2
	report.URLsChanged = 1
	report.TitlesAdded = 2
	report.TagsAdded = 3
	report.ParenthesesFixed = 1
	report.Redirects = 2
	report.TitlesFetched = 2
	report.TitlesFailed = 1
	report.TagsFetched = 3
	report.TagsFailed = 2
	report.UpdatesPerformed = 3
	report.DeletionsPerformed = 1
	report.ApplyErrors = 2
	report.StatusCodes[opURLValidation] = map[int]int{200: 5, 404: 1}

	output := render(report.Print)

	for _, want := range []string{
		"Skipped (unchecked):2",
		"Tags added:         3",
		"URLs changed:       1",
		"Titles added:       2",
		"Parentheses fixed:  1",
		"Redirects followed: 2",
		"Deletions performed: 1",
		"Writes rejected:    2",
		// printFetchStats renders the failure count only when there is one.
		"Titles fetched:     2 (1 failed)",
		"Tags fetched:       3 (2 failed)",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("report is missing %q\n--- output ---\n%s", want, output)
		}
	}
}

// A status code net/http has no text for must not render an empty label.
func TestPrint_UnknownStatusCode(t *testing.T) {
	t.Parallel()

	report := newStatistics()
	report.TotalBookmarks = 1
	report.StatusCodes[opTitleFetch] = map[int]int{599: 1}

	if output := render(report.Print); !strings.Contains(output, "599 Unknown: 1") {
		t.Errorf("report is missing the unknown-status line\n--- output ---\n%s", output)
	}
}

// The URL PROCESSING block prints only the counters that moved, so a run with
// redirects but no parenthesis repairs carries no zero line.
func TestPrint_URLProcessingShowsOnlyMovedCounters(t *testing.T) {
	t.Parallel()

	report := newStatistics()
	report.TotalBookmarks = 1
	report.Redirects = 1

	output := render(report.Print)

	if strings.Contains(output, "Parentheses fixed") {
		t.Errorf("report shows a zero parentheses line\n--- output ---\n%s", output)
	}
	if !strings.Contains(output, "Redirects followed: 1") {
		t.Errorf("report is missing the redirect line\n--- output ---\n%s", output)
	}
}

// Sorted iteration means the same counters render identically every time. The
// report used to range over maps directly, so two runs could not be diffed.
func TestPrint_IsDeterministic(t *testing.T) {
	t.Parallel()

	build := func() *Statistics {
		report := newStatistics()
		report.TotalBookmarks = 3
		report.StatusCodes[opURLValidation] = map[int]int{200: 1, 301: 2, 404: 3}
		report.StatusCodes[opTitleFetch] = map[int]int{200: 4, 500: 5}
		report.StatusCodes[opTagFetch] = map[int]int{200: 6}

		return report
	}

	first := render(build().Print)
	for range 5 {
		if got := render(build().Print); got != first {
			t.Fatalf("report differs between runs\n--- first ---\n%s\n--- later ---\n%s",
				first, got)
		}
	}

	// Ordering is the property under test, so pin it explicitly.
	titleAt := strings.Index(first, "Title Fetch")
	urlAt := strings.Index(first, "URL Validation")
	if titleAt < 0 || urlAt < 0 || titleAt > urlAt {
		t.Errorf("operations are not in sorted order\n--- output ---\n%s", first)
	}
	if strings.Index(first, "200 OK") > strings.Index(first, "500 Internal Server Error") {
		t.Errorf("status codes are not in ascending order\n--- output ---\n%s", first)
	}
}

// -verbose diagnostics must name the bookmark and the reason, and must go to
// stderr so they cannot interleave with the statistics block on stdout.
func TestVerbose_SkipDiagnosticNamesURLAndReason(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.verbose = true

	action := chk.recordSkip(Bookmark{URL: testURL}, errors.New("scheme not dialable"))

	output := stderrOf(chk)
	if action.Action != ActionNoChange {
		t.Errorf("recordSkip() action = %v, want ActionNoChange", action.Action)
	}
	if !strings.Contains(output, testURL) {
		t.Errorf("stderr = %q, want it to name the bookmark", output)
	}
	if !strings.Contains(output, "scheme not dialable") {
		t.Errorf("stderr = %q, want it to carry the reason", output)
	}
	if stdoutOf(chk) != "" {
		t.Errorf("stdout = %q, want the diagnostic kept off the report stream", stdoutOf(chk))
	}
}

func TestVerbose_ApplyErrorNamesOperationAndURL(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.verbose = true

	chk.reportApplyError("deleting", testURL, errors.New("boom"))

	for _, want := range []string{"deleting", testURL, "boom"} {
		if output := stderrOf(chk); !strings.Contains(output, want) {
			t.Errorf("stderr = %q, want it to contain %q", output, want)
		}
	}
}

// The progress bar is built only outside CI mode. Its absence must not change
// the actions produced.
func TestProcessBookmarksParallel_ProgressBarDoesNotChangeResults(t *testing.T) {
	t.Parallel()

	server := staticServer(t, http.StatusOK, "")

	bookmarks := make([]Bookmark, 3)
	for i := range bookmarks {
		bookmarks[i] = Bookmark{URL: server.URL, Title: "t", Tags: "x"}
	}

	for _, ciMode := range []bool{false, true} {
		chk := newTestChecker(t)
		chk.ciMode = ciMode
		chk.workers = 2

		if actions := chk.processBookmarksParallel(bookmarks); len(actions) != len(bookmarks) {
			t.Errorf("ciMode %v: got %d actions, want %d", ciMode, len(actions), len(bookmarks))
		}
	}
}

// barLabel is the description every progress bar test draws with.
const barLabel = "Work"

// The line carries the description, percentage, bar, count, and the elapsed and
// remaining time, the layout the progressbar module drew.
func TestProgressBar_Line(t *testing.T) {
	t.Parallel()

	bar := &progressBar{description: barLabel, total: 4, done: 2}

	want := "Work  50% |" + strings.Repeat("█", 20) + strings.Repeat(" ", 20) + "| (2/4) [3s:3s]"
	if got := bar.line(3 * time.Second); got != want {
		t.Errorf("line() = %q\nwant     %q", got, want)
	}

	// Before the first item finishes there is no rate to estimate from.
	bar.done = 0
	if got := bar.line(0); !strings.HasSuffix(got, "  0% |"+strings.Repeat(" ", 40)+
		"| (0/4) [0s:?]") {
		t.Errorf("line() = %q, want an empty bar and an unknown remaining time", got)
	}
}

// Each increment redraws in place with "\r", and the last one ends the line so
// the next bar does not draw over this one.
func TestProgressBar_IncrementRedrawsAndEndsTheLine(t *testing.T) {
	t.Parallel()

	out := &lockedBuffer{}
	bar := &progressBar{out: out, description: barLabel, total: 2, start: time.Now()}

	bar.increment()
	bar.increment()

	output := out.String()
	if frames := strings.Count(output, "\r"); frames != 2 {
		t.Errorf("drew %d frames, want 2: %q", frames, output)
	}
	if !strings.HasSuffix(output, "\n") || strings.Count(output, "\n") != 1 {
		t.Errorf("output = %q, want exactly one newline, at the end", output)
	}
	if !strings.Contains(output, "(2/2)") {
		t.Errorf("output = %q, want the final count", output)
	}
}

// A frame shorter than the widest one so far is padded to it, so no tail of the
// earlier, longer frame is left on screen.
func TestProgressBar_PadsShorterFrames(t *testing.T) {
	t.Parallel()

	out := &lockedBuffer{}
	bar := &progressBar{out: out, description: barLabel, total: 2, start: time.Now(), longest: 120}

	bar.increment()

	if width := utf8.RuneCountInString(strings.TrimPrefix(out.String(), "\r")); width != 120 {
		t.Errorf("frame is %d runes wide, want it padded to 120", width)
	}

	// The width a fresh bar pads to is the width of what it has drawn.
	fresh := &lockedBuffer{}
	tracked := &progressBar{out: fresh, description: barLabel, total: 2, start: time.Now()}

	tracked.increment()

	drawn := utf8.RuneCountInString(strings.TrimPrefix(fresh.String(), "\r"))
	if drawn == 0 || tracked.longest != drawn {
		t.Errorf("longest = %d after drawing a %d-rune frame, want them equal",
			tracked.longest, drawn)
	}
}

// The worker pool increments from every goroutine; under -race this fails if
// the bar's state is not guarded.
func TestProgressBar_ConcurrentIncrements(t *testing.T) {
	t.Parallel()

	out := &lockedBuffer{}
	bar := &progressBar{out: out, description: barLabel, total: 50, start: time.Now()}

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(bar.increment)
	}
	wg.Wait()

	output := out.String()
	if !strings.Contains(output, "(50/50)") || strings.Count(output, "\n") != 1 {
		t.Errorf("output ends %q, want the full count and one newline",
			output[max(0, len(output)-120):])
	}
}

// A nil bar is what -ci gets, and the call sites rely on it ignoring
// increment and breakLine rather than checking for nil themselves.
func TestProgressBar_NilIgnoresCalls(t *testing.T) {
	t.Parallel()

	var bar *progressBar

	bar.increment()
	bar.breakLine()
}

// breakLine writes a newline only while a frame is on screen: not before the
// first increment, which has drawn nothing, and not after the last, which has
// already ended the line.
func TestProgressBar_BreakLineOnlyMidBar(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		done  int
		every time.Duration
		want  string
	}{
		{done: 0, want: ""},
		{done: 1, want: "\n"},
		{done: 2, want: ""},
		// Report mode draws no frame, so there is no line to break.
		{done: 1, every: progressReportInterval, want: ""},
	} {
		out := &lockedBuffer{}
		bar := &progressBar{
			out: out, description: barLabel, total: 2, done: testCase.done, every: testCase.every,
		}

		bar.breakLine()

		if got := out.String(); got != testCase.want {
			t.Errorf("breakLine() at %d/2 (every %s) wrote %q, want %q",
				testCase.done, testCase.every, got, testCase.want)
		}
	}
}

// In report mode the bar prints a whole line, never a frame, and at most once
// per interval: a second item finishing straight after the first adds nothing,
// and the next line comes once another interval has passed.
func TestProgressBar_ReportsALineEachInterval(t *testing.T) {
	t.Parallel()

	out := &lockedBuffer{}
	bar := &progressBar{
		out: out, description: barLabel, total: 4, every: progressReportInterval,
		start: time.Now().Add(-progressReportInterval - time.Second),
	}

	bar.increment()
	bar.increment()

	// Move the start back instead of sleeping: the interval has now passed again.
	bar.start = bar.start.Add(-progressReportInterval)
	bar.increment()

	output := out.String()
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("printed %d lines, want 2: %q", len(lines), output)
	}
	if !strings.HasPrefix(lines[0], barLabel+" 1/4 (25%), ") ||
		!strings.Contains(lines[0], " elapsed, ") || !strings.HasSuffix(lines[0], " left") {
		t.Errorf("first line = %q, want the count, the elapsed time and the estimate", lines[0])
	}
	if !strings.HasPrefix(lines[1], barLabel+" 3/4 (75%)") {
		t.Errorf("second line = %q, want the count after the third item", lines[1])
	}
	if strings.ContainsAny(output, "\r█") {
		t.Errorf("output = %q, want no frame in report mode", output)
	}
}

// With stdout and stderr on one terminal, a deletion record started right
// after the apply bar's frame. Every record must begin a line, as the README's
// example shows, so it can be copied back to posts/add as it stands.
func TestApplyBookmarkActions_DeletionRecordStartsItsOwnLine(t *testing.T) {
	t.Parallel()

	screen := &lockedBuffer{}
	chk := newTestChecker(t)
	chk.out, chk.errOut = screen, screen
	chk.pinboardEndpoint = okServer(t).URL

	actions := make([]BookmarkAction, 3)
	for i := range actions {
		actions[i] = BookmarkAction{
			Original: Bookmark{URL: fmt.Sprintf("http://gone.example/%d", i)},
			Action:   ActionDelete,
		}
	}

	chk.applyBookmarkActions(actions)

	output := screen.String()
	if records := strings.Count(output, "deleted ("); records != 3 {
		t.Fatalf("wrote %d deletion records, want 3: %q", records, output)
	}
	for line := range strings.SplitSeq(output, "\n") {
		if i := strings.Index(line, "deleted ("); i > 0 {
			t.Errorf("record starts at column %d, after %q", i, line[:i])
		}
	}
}

// The bar is off under -ci and when there is nothing to count.
func TestNewProgressBar_OffUnderCIAndForNothing(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)

	if bar := chk.newProgressBar(3, barLabel); bar == nil {
		t.Error("newProgressBar() = nil, want a bar outside -ci")
	}
	if bar := chk.newProgressBar(0, barLabel); bar != nil {
		t.Error("newProgressBar(0) is a bar, want nil when there is nothing to count")
	}

	chk.ciMode = true
	if bar := chk.newProgressBar(3, barLabel); bar != nil {
		t.Error("newProgressBar() is a bar under -ci, want nil")
	}
}

// Under -verbose the bar reports lines instead of drawing frames, and -ci still
// turns it off when both are given.
func TestNewProgressBar_ReportsUnderVerbose(t *testing.T) {
	t.Parallel()

	chk := newTestChecker(t)
	chk.verbose = true

	bar := chk.newProgressBar(3, barLabel)
	if bar == nil || bar.every != progressReportInterval {
		t.Fatalf("newProgressBar() under -verbose = %+v, want one reporting every %s",
			bar, progressReportInterval)
	}

	chk.ciMode = true
	if bar := chk.newProgressBar(3, barLabel); bar != nil {
		t.Error("newProgressBar() is a bar under -ci -verbose, want nil")
	}
}

// With -verbose, the narration of each bookmark landed right after the frame
// drawn before it. No frame may be drawn in that mode, so every narration line
// starts a line of its own.
func TestProcessBookmarksParallel_VerboseDrawsNoFrames(t *testing.T) {
	t.Parallel()

	server := staticServer(t, http.StatusOK, "<html><head><title>x</title></head></html>")

	screen := &lockedBuffer{}
	chk := newTestChecker(t)
	chk.out, chk.errOut = screen, screen
	chk.verbose = true
	chk.workers = 1

	bookmarks := make([]Bookmark, 3)
	for i := range bookmarks {
		bookmarks[i] = Bookmark{URL: fmt.Sprintf("%s/%d", server.URL, i), Title: "t", Tags: "x"}
	}

	chk.processBookmarksParallel(bookmarks)

	output := screen.String()
	if strings.Contains(output, "\r") {
		t.Errorf("output = %q, want no frame under -verbose", output)
	}
	starts := 0
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, "Processing: ") {
			starts++
		}
	}
	if starts != 3 {
		t.Errorf("found %d narration lines at a line start, want 3: %q", starts, output)
	}
}

// interleaveHosts deals bookmarks round-robin across hosts and keeps each
// host's own order, so a run of one host's bookmarks no longer parks the pool.
func TestInterleaveHosts(t *testing.T) {
	t.Parallel()

	got := interleaveHosts([]Bookmark{
		{URL: "http://a.example/1"}, {URL: "http://a.example/2"}, {URL: "http://a.example/3"},
		{URL: "http://b.example/1"}, {URL: testBadURL}, {URL: "http://a.example/4"},
	})

	want := []string{
		"http://a.example/1", "http://b.example/1", testBadURL,
		"http://a.example/2", "http://a.example/3", "http://a.example/4",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d bookmarks, want %d", len(got), len(want))
	}
	for i, bookmark := range got {
		if bookmark.URL != want[i] {
			t.Errorf("position %d = %q, want %q", i, bookmark.URL, want[i])
		}
	}
}

// The pool must not spend its whole width waiting on one host. Twenty
// bookmarks on host A ahead of one on host B used to delay B by every slot A
// had reserved; interleaved, B goes out with the first wave.
func TestProcessBookmarksParallel_OneHostDoesNotParkThePool(t *testing.T) {
	t.Parallel()

	const interval = 100 * time.Millisecond

	var (
		mu    sync.Mutex
		start time.Time
		bAt   time.Duration
	)

	hostA := staticServer(t, http.StatusOK, "")
	hostB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		bAt = time.Since(start)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hostB.Close)

	bookmarks := make([]Bookmark, 0, 21)
	for i := range 20 {
		bookmarks = append(bookmarks, Bookmark{URL: fmt.Sprintf("%s/p%d", hostA.URL, i),
			Title: "t", Tags: "x"})
	}
	bookmarks = append(bookmarks, Bookmark{URL: hostB.URL + "/only", Title: "t", Tags: "x"})

	chk := newTestChecker(t)
	chk.ciMode = true
	chk.hosts = newHostThrottle(interval)

	mu.Lock()
	start = time.Now()
	mu.Unlock()

	chk.processBookmarksParallel(bookmarks)

	mu.Lock()
	defer mu.Unlock()

	if bAt >= interval {
		t.Errorf("host B was first requested after %s, want under %s", bAt, interval)
	}
}

// The -verbose narration on stdout must name the bookmark at every stage it
// reports, so a run can be followed without guessing which URL a line refers to.
func TestVerbose_NarratesProcessingToStdout(t *testing.T) {
	t.Parallel()

	page := staticServer(t, http.StatusOK,
		"<html><head><title>Fetched Page</title></head></html>")

	chk := newTestChecker(t)
	chk.verbose = true
	chk.pinboardEndpoint = staticServer(t, http.StatusOK, `[{"popular":["go"]}]`).URL

	action := chk.processBookmark(Bookmark{URL: page.URL})
	if action.Action != ActionUpdate {
		t.Fatalf("processBookmark() action = %v, want ActionUpdate", action.Action)
	}

	output := stdoutOf(chk)
	for _, want := range []string{
		"Processing: " + page.URL,
		"Fetched title: Fetched Page",
		"Fetched tags: go," + autoTagMarker,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("stdout is missing %q\n--- output ---\n%s", want, output)
		}
	}
}

// The parenthesis repair narrates both the attempt and the result, naming the
// URL on each side of the rewrite.
func TestVerbose_NarratesParenthesisRepair(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ")") {
				w.WriteHeader(http.StatusNotFound)

				return
			}
			w.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(server.Close)

	withParen := server.URL + "/page)"

	chk := newTestChecker(t)
	chk.verbose = true

	if got := chk.fixParenthesesSuffix(withParen); got != server.URL+"/page" {
		t.Errorf("fixParenthesesSuffix() = %q, want the trimmed URL", got)
	}

	output := stdoutOf(chk)
	if !strings.Contains(output, "ends with ')'") {
		t.Errorf("stdout = %q, want the retry notice", output)
	}
	if !strings.Contains(output, withParen+"' -> '"+server.URL+"/page") {
		t.Errorf("stdout = %q, want both sides of the rewrite", output)
	}
}

func TestVerbose_NarratesRedirect(t *testing.T) {
	t.Parallel()

	final := staticServer(t, http.StatusOK, "")

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", final.URL)
			w.WriteHeader(http.StatusMovedPermanently)
		}))
	t.Cleanup(server.Close)

	chk := newTestChecker(t)
	chk.verbose = true

	if _, err := chk.followRedirects(server.URL); err != nil {
		t.Errorf("followRedirects() error = %v, want nil", err)
	}

	if output := stdoutOf(chk); !strings.Contains(output, final.URL) {
		t.Errorf("stdout = %q, want it to name the redirect destination", output)
	}
}

// The 429 notice must say which endpoint was throttled and what the interval
// became, so a slow run is diagnosable.
func TestVerbose_NarratesRateLimitBackoff(t *testing.T) {
	t.Parallel()

	attempts := 0
	server := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}
		w.WriteHeader(http.StatusOK)
		writeBody(t, w, `[]`)
	})

	chk := newTestChecker(t)
	chk.verbose = true
	chk.pinboardEndpoint = server.URL

	if _, err := chk.getBookmarks(); err != nil {
		t.Errorf("getBookmarks() error = %v, want nil", err)
	}

	output := stderrOf(chk)
	if !strings.Contains(output, "/posts/all") {
		t.Errorf("stderr = %q, want it to name the throttled endpoint", output)
	}
	if !strings.Contains(output, "interval now") {
		t.Errorf("stderr = %q, want it to report the widened interval", output)
	}
}

// A transport error that is not a *url.Error still gets a usable message.
func TestSanitizeTransportError_NonURLError(t *testing.T) {
	t.Parallel()

	err := sanitizeTransportError("/posts/all", errors.New("plain failure"))
	if !strings.Contains(err.Error(), "/posts/all") {
		t.Errorf("error = %v, want it to name the path", err)
	}
	if !strings.Contains(err.Error(), "plain failure") {
		t.Errorf("error = %v, want it to carry the cause", err)
	}
}
