package main

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
)

// newStatistics builds a Statistics with its map ready. A nil map panics on
// write, so construction lives here rather than being repeated at each literal.
func newStatistics() *Statistics {
	return &Statistics{StatusCodes: make(map[string]map[int]int)}
}

type Statistics struct {
	mu sync.Mutex

	// Timing
	FetchBookmarksStart    time.Time
	FetchBookmarksDuration time.Duration
	ProcessingStart        time.Time
	ProcessingDuration     time.Duration
	ApplicationStart       time.Time
	ApplicationDuration    time.Duration
	TotalDuration          time.Duration

	// Action counts
	TotalBookmarks int
	ActionNoChange int
	ActionUpdate   int
	ActionDelete   int

	// Update breakdown
	URLsChanged int
	TitlesAdded int
	TagsAdded   int

	// URL processing
	ParenthesesFixed int
	Redirects        int

	// HTTP status codes by operation type
	StatusCodes map[string]map[int]int // operation -> status code -> count

	// API operations
	TitlesFetched int
	TitlesFailed  int
	TagsFetched   int
	TagsFailed    int
	// TitlesEmpty and TagsEmpty count calls that succeeded and had nothing to
	// add — a page with no <title>, a URL Pinboard has no suggestions for.
	// Kept apart from the Failed counters, which previously absorbed them and
	// reported hundreds of failures on a run where nothing failed.
	TitlesEmpty        int
	TagsEmpty          int
	UpdatesPerformed   int
	DeletionsPerformed int

	// Errors
	// Skipped counts bookmarks this tool could not verify — an undialable
	// scheme, a timeout, a DNS failure. They are left untouched; the count
	// exists so a run says how much it declined to judge instead of staying
	// silent about it.
	Skipped int
	// ApplyErrors counts writes Pinboard rejected. Without it a non-verbose run
	// reported only successes and exited 0, so a partly failed run looked clean.
	ApplyErrors int
	// Merged counts bookmarks whose new URL was one the account already held,
	// so the duplicate was removed and the existing entry left alone. Counted
	// separately from Updates because nothing was written: reporting a merge as
	// an update hid the fact that a second copy of a page had existed.
	Merged int
}

// RecordMerged books a duplicate collapsed into a bookmark that already existed
// at the destination URL.
func (s *Statistics) RecordMerged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Merged++
}

// RecordSkipped books a bookmark that was left alone because it could not be
// checked.
func (s *Statistics) RecordSkipped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Skipped++
}

// RecordApplyError books a write Pinboard refused.
func (s *Statistics) RecordApplyError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ApplyErrors++
}

// ApplyErrorCount reports how many writes failed, so the caller can set a
// non-zero exit status.
func (s *Statistics) ApplyErrorCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ApplyErrors
}

// CISummary is the one line -ci prints in place of the report. It counts what
// was done rather than what was planned, so a dry run reports zero writes.
func (s *Statistics) CISummary() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return fmt.Sprintf("updated %d, deleted %d, merged %d, skipped %d, rejected %d",
		s.UpdatesPerformed, s.DeletionsPerformed, s.Merged, s.Skipped, s.ApplyErrors)
}

func (s *Statistics) RecordAction(action BookmarkAction) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch action.Action {
	case ActionNoChange:
		s.ActionNoChange++
	case ActionUpdate:
		s.ActionUpdate++
		if action.NewURL != "" && action.NewURL != action.Original.URL {
			s.URLsChanged++
		}
		if action.NewTitle != "" {
			s.TitlesAdded++
		}
		if action.NewTags != "" {
			s.TagsAdded++
		}
	case ActionDelete:
		s.ActionDelete++
	}
}

func (s *Statistics) RecordHTTPStatus(operation string, statusCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.StatusCodes[operation] == nil {
		s.StatusCodes[operation] = make(map[int]int)
	}
	s.StatusCodes[operation][statusCode]++
}

func (s *Statistics) IncrementParenthesesFixed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ParenthesesFixed++
}

func (s *Statistics) IncrementRedirects() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Redirects++
}

// RecordTitleFetch books a title lookup as one of three outcomes.
//
// Empty-but-no-error is its own outcome, not a failure: a page with no <title>
// is a successful fetch with nothing to add. Folding it into TitlesFailed made
// a completely healthy run print "Titles fetched: 12 (188 failed)" directly
// under an HTTP block showing 200 responses throughout.
func (s *Statistics) RecordTitleFetch(found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case err != nil:
		s.TitlesFailed++
	case found:
		s.TitlesFetched++
	default:
		s.TitlesEmpty++
	}
}

// RecordTagFetch books a tag lookup as one of three outcomes; see
// RecordTitleFetch. Pinboard having no suggestions for a URL is the common
// case, not an error — getSuggestedTags returns an empty string for it so the
// bookmark is left alone.
func (s *Statistics) RecordTagFetch(found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case err != nil:
		s.TagsFailed++
	case found:
		s.TagsFetched++
	default:
		s.TagsEmpty++
	}
}

func (s *Statistics) IncrementUpdates() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.UpdatesPerformed++
}

func (s *Statistics) IncrementDeletions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DeletionsPerformed++
}

func (s *Statistics) printTiming(w io.Writer) {
	fmt.Fprintln(w, "⏱  TIMING")
	fmt.Fprintf(w, "   Fetch bookmarks:    %.3fs\n", s.FetchBookmarksDuration.Seconds())
	avgProcessing := 0.0
	if s.TotalBookmarks > 0 {
		avgProcessing = s.ProcessingDuration.Seconds() / float64(s.TotalBookmarks)
	}
	fmt.Fprintf(w, "   Process bookmarks:  %.3fs (avg %.3fs per bookmark)\n",
		s.ProcessingDuration.Seconds(), avgProcessing)
	fmt.Fprintf(w, "   Apply changes:      %.3fs\n", s.ApplicationDuration.Seconds())
	fmt.Fprintf(w, "   Total duration:     %.3fs\n", s.TotalDuration.Seconds())
	fmt.Fprintln(w)
}

func (s *Statistics) printActions(w io.Writer) {
	fmt.Fprintln(w, "📊 ACTIONS")
	fmt.Fprintf(w, "   Total bookmarks:    %d\n", s.TotalBookmarks)
	noChangePercent := 0.0
	updatePercent := 0.0
	deletePercent := 0.0
	if s.TotalBookmarks > 0 {
		noChangePercent = float64(s.ActionNoChange) / float64(s.TotalBookmarks) * 100
		updatePercent = float64(s.ActionUpdate) / float64(s.TotalBookmarks) * 100
		deletePercent = float64(s.ActionDelete) / float64(s.TotalBookmarks) * 100
	}
	fmt.Fprintf(w, "   No changes:         %d (%.1f%%)\n", s.ActionNoChange, noChangePercent)
	fmt.Fprintf(w, "   Updates:            %d (%.1f%%)\n", s.ActionUpdate, updatePercent)
	fmt.Fprintf(w, "   Deletions:          %d (%.1f%%)\n", s.ActionDelete, deletePercent)
	if s.Skipped > 0 {
		fmt.Fprintf(w, "   %-20s%d\n", "Skipped (unchecked):", s.Skipped)
	}
	fmt.Fprintln(w)
}

func (s *Statistics) printUpdatesBreakdown(w io.Writer) {
	if s.URLsChanged == 0 && s.TitlesAdded == 0 && s.TagsAdded == 0 {
		return
	}

	fmt.Fprintln(w, "📝 UPDATES BREAKDOWN")
	if s.URLsChanged > 0 {
		fmt.Fprintf(w, "   URLs changed:       %d\n", s.URLsChanged)
	}
	if s.TitlesAdded > 0 {
		fmt.Fprintf(w, "   Titles added:       %d\n", s.TitlesAdded)
	}
	if s.TagsAdded > 0 {
		fmt.Fprintf(w, "   Tags added:         %d\n", s.TagsAdded)
	}
	fmt.Fprintln(w)
}

func (s *Statistics) printURLProcessing(w io.Writer) {
	if s.ParenthesesFixed == 0 && s.Redirects == 0 {
		return
	}

	fmt.Fprintln(w, "🔗 URL PROCESSING")

	if s.ParenthesesFixed > 0 {
		fmt.Fprintf(w, "   Parentheses fixed:  %d\n", s.ParenthesesFixed)
	}
	if s.Redirects > 0 {
		fmt.Fprintf(w, "   Redirects followed: %d\n", s.Redirects)
	}
	fmt.Fprintln(w)
}

// printHTTPResponses renders the per-operation status breakdown.
//
// Operations and status codes are sorted before printing: ranging a map
// directly randomizes the order, so two runs over an unchanged account produced
// textually different reports that could not be diffed.
func (s *Statistics) printHTTPResponses(w io.Writer) {
	if len(s.StatusCodes) == 0 {
		return
	}

	fmt.Fprintln(w, "🌐 HTTP RESPONSES")
	for _, operation := range slices.Sorted(maps.Keys(s.StatusCodes)) {
		printHTTPOperation(w, operation, s.StatusCodes[operation])
	}
	fmt.Fprintln(w)
}

func printHTTPOperation(w io.Writer, operation string, codes map[int]int) {
	operationName := operation
	switch operation {
	case opURLValidation:
		operationName = "URL Validation"
	case opTitleFetch:
		operationName = "Title Fetch"
	case opTagFetch:
		operationName = "Tag Fetch"
	}

	fmt.Fprintf(w, "   %s:\n", operationName)

	for _, statusCode := range slices.Sorted(maps.Keys(codes)) {
		statusText := http.StatusText(statusCode)
		if statusText == "" {
			statusText = "Unknown"
		}
		fmt.Fprintf(w, "     • %d %s: %d\n", statusCode, statusText, codes[statusCode])
	}
}

func (s *Statistics) printAPIOperations(w io.Writer) {
	if s.TitlesFetched == 0 && s.TagsFetched == 0 &&
		s.UpdatesPerformed == 0 && s.DeletionsPerformed == 0 &&
		s.TitlesFailed == 0 && s.TagsFailed == 0 && s.ApplyErrors == 0 &&
		s.TitlesEmpty == 0 && s.TagsEmpty == 0 && s.Merged == 0 {
		return
	}

	fmt.Fprintln(w, "⚙️  API OPERATIONS")
	printFetchStats(w, "Titles fetched", s.TitlesFetched, s.TitlesEmpty, s.TitlesFailed)
	printFetchStats(w, "Tags fetched", s.TagsFetched, s.TagsEmpty, s.TagsFailed)

	if s.UpdatesPerformed > 0 {
		fmt.Fprintf(w, "   Updates performed:  %d\n", s.UpdatesPerformed)
	}
	if s.DeletionsPerformed > 0 {
		fmt.Fprintf(w, "   Deletions performed: %d\n", s.DeletionsPerformed)
	}
	if s.Merged > 0 {
		fmt.Fprintf(w, "   %-20s%d\n", "Duplicates merged:", s.Merged)
	}
	if s.ApplyErrors > 0 {
		fmt.Fprintf(w, "   Writes rejected:    %d\n", s.ApplyErrors)
	}
	fmt.Fprintln(w)
}

// printFetchStats renders one "N fetched" line with its qualifiers, or nothing
// when the operation never ran. The %-20s padding replaces hand-counted
// spacing, which had already drifted out of alignment for the longer labels.
//
// empty and failed are printed separately because they mean different things:
// nothing to add is a successful call, a failure is not, and merging them told
// the reader a healthy run had gone wrong.
func printFetchStats(w io.Writer, label string, fetched, empty, failed int) {
	if fetched == 0 && empty == 0 && failed == 0 {
		return
	}

	fmt.Fprintf(w, "   %-20s%d", label+":", fetched)
	if empty > 0 {
		fmt.Fprintf(w, " (%d with nothing to add)", empty)
	}
	if failed > 0 {
		fmt.Fprintf(w, " (%d failed)", failed)
	}
	fmt.Fprintln(w)
}

// Print renders the report to w. It takes the writer rather than printing to
// stdout so a test reads the report from a buffer instead of swapping the
// process-wide os.Stdout, which kept every report test out of t.Parallel.
func (s *Statistics) Print(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Fprintln(w, "════════════════════════════════════════════════════════")
	fmt.Fprintln(w, "                   PROCESSING STATISTICS")
	fmt.Fprintln(w, "════════════════════════════════════════════════════════")
	fmt.Fprintln(w)

	s.printTiming(w)
	s.printActions(w)
	s.printUpdatesBreakdown(w)
	s.printURLProcessing(w)
	s.printHTTPResponses(w)
	s.printAPIOperations(w)

	fmt.Fprintln(w, "════════════════════════════════════════════════════════")
}
