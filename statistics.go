package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

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
	ShortURLsExpanded map[string]int // "bitly", "tinyurl", "isgd"
	ShortURLsFailed   map[string]int
	ParenthesesFixed  int
	Redirects         int

	// HTTP status codes by operation type
	StatusCodes map[string]map[int]int // operation -> status code -> count

	// API operations
	TitlesFetched      int
	TitlesFailed       int
	TagsFetched        int
	TagsFailed         int
	UpdatesPerformed   int
	DeletionsPerformed int

	// Errors
	TotalErrors int
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
		s.TotalErrors++
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

func (s *Statistics) RecordShortenerExpansion(service string, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if success {
		s.ShortURLsExpanded[service]++
	} else {
		s.ShortURLsFailed[service]++
	}
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

func (s *Statistics) RecordTitleFetch(success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if success {
		s.TitlesFetched++
	} else {
		s.TitlesFailed++
	}
}

func (s *Statistics) RecordTagFetch(success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if success {
		s.TagsFetched++
	} else {
		s.TagsFailed++
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

func (s *Statistics) printTiming() {
	fmt.Println("⏱  TIMING")
	fmt.Printf("   Fetch bookmarks:    %.3fs\n", s.FetchBookmarksDuration.Seconds())
	avgProcessing := 0.0
	if s.TotalBookmarks > 0 {
		avgProcessing = s.ProcessingDuration.Seconds() / float64(s.TotalBookmarks)
	}
	fmt.Printf("   Process bookmarks:  %.3fs (avg %.3fs per bookmark)\n",
		s.ProcessingDuration.Seconds(), avgProcessing)
	fmt.Printf("   Apply changes:      %.3fs\n", s.ApplicationDuration.Seconds())
	fmt.Printf("   Total duration:     %.3fs\n", s.TotalDuration.Seconds())
	fmt.Println()
}

func (s *Statistics) printActions() {
	fmt.Println("📊 ACTIONS")
	fmt.Printf("   Total bookmarks:    %d\n", s.TotalBookmarks)
	noChangePercent := 0.0
	updatePercent := 0.0
	deletePercent := 0.0
	if s.TotalBookmarks > 0 {
		noChangePercent = float64(s.ActionNoChange) / float64(s.TotalBookmarks) * 100
		updatePercent = float64(s.ActionUpdate) / float64(s.TotalBookmarks) * 100
		deletePercent = float64(s.ActionDelete) / float64(s.TotalBookmarks) * 100
	}
	fmt.Printf("   No changes:         %d (%.1f%%)\n", s.ActionNoChange, noChangePercent)
	fmt.Printf("   Updates:            %d (%.1f%%)\n", s.ActionUpdate, updatePercent)
	fmt.Printf("   Deletions:          %d (%.1f%%)\n", s.ActionDelete, deletePercent)
	fmt.Println()
}

func (s *Statistics) printUpdatesBreakdown() {
	if s.URLsChanged == 0 && s.TitlesAdded == 0 && s.TagsAdded == 0 {
		return
	}

	fmt.Println("📝 UPDATES BREAKDOWN")
	if s.URLsChanged > 0 {
		fmt.Printf("   URLs changed:       %d\n", s.URLsChanged)
	}
	if s.TitlesAdded > 0 {
		fmt.Printf("   Titles added:       %d\n", s.TitlesAdded)
	}
	if s.TagsAdded > 0 {
		fmt.Printf("   Tags added:         %d\n", s.TagsAdded)
	}
	fmt.Println()
}

func (s *Statistics) printURLProcessing() {
	totalExpanded := 0
	for _, count := range s.ShortURLsExpanded {
		totalExpanded += count
	}
	totalFailed := 0
	for _, count := range s.ShortURLsFailed {
		totalFailed += count
	}

	if totalExpanded == 0 && totalFailed == 0 && s.ParenthesesFixed == 0 && s.Redirects == 0 {
		return
	}

	fmt.Println("🔗 URL PROCESSING")
	s.printShortenerStats(totalExpanded, totalFailed)

	if s.ParenthesesFixed > 0 {
		fmt.Printf("   Parentheses fixed:  %d\n", s.ParenthesesFixed)
	}
	if s.Redirects > 0 {
		fmt.Printf("   Redirects followed: %d\n", s.Redirects)
	}
	fmt.Println()
}

func (s *Statistics) printShortenerStats(totalExpanded, totalFailed int) {
	if totalExpanded == 0 && totalFailed == 0 {
		return
	}

	fmt.Println("   Short URLs expanded:")
	if s.ShortURLsExpanded["bitly"] > 0 || s.ShortURLsFailed["bitly"] > 0 {
		fmt.Printf("     • bit.ly:         %d success, %d failed\n",
			s.ShortURLsExpanded["bitly"], s.ShortURLsFailed["bitly"])
	}
	if s.ShortURLsExpanded["tinyurl"] > 0 || s.ShortURLsFailed["tinyurl"] > 0 {
		fmt.Printf("     • tinyurl:        %d success, %d failed\n",
			s.ShortURLsExpanded["tinyurl"], s.ShortURLsFailed["tinyurl"])
	}
	if s.ShortURLsExpanded["isgd"] > 0 || s.ShortURLsFailed["isgd"] > 0 {
		fmt.Printf("     • is.gd:          %d success, %d failed\n",
			s.ShortURLsExpanded["isgd"], s.ShortURLsFailed["isgd"])
	}
}

func (s *Statistics) printHTTPResponses() {
	if len(s.StatusCodes) == 0 {
		return
	}

	fmt.Println("🌐 HTTP RESPONSES")
	for operation, codes := range s.StatusCodes {
		s.printHTTPOperation(operation, codes)
	}
	fmt.Println()
}

func (s *Statistics) printHTTPOperation(operation string, codes map[int]int) {
	operationName := operation
	switch operation {
	case "url-validation":
		operationName = "URL Validation"
	case "title-fetch":
		operationName = "Title Fetch"
	case "tag-fetch":
		operationName = "Tag Fetch"
	}

	fmt.Printf("   %s:\n", operationName)
	for statusCode, count := range codes {
		statusText := http.StatusText(statusCode)
		if statusText == "" {
			statusText = "Unknown"
		}
		fmt.Printf("     • %d %s: %d\n", statusCode, statusText, count)
	}
}

func (s *Statistics) printAPIOperations() {
	if s.TitlesFetched == 0 && s.TagsFetched == 0 &&
		s.UpdatesPerformed == 0 && s.DeletionsPerformed == 0 &&
		s.TitlesFailed == 0 && s.TagsFailed == 0 {
		return
	}

	fmt.Println("⚙️  API OPERATIONS")
	s.printTitleStats()
	s.printTagStats()

	if s.UpdatesPerformed > 0 {
		fmt.Printf("   Updates performed:  %d\n", s.UpdatesPerformed)
	}
	if s.DeletionsPerformed > 0 {
		fmt.Printf("   Deletions performed: %d\n", s.DeletionsPerformed)
	}
	fmt.Println()
}

func (s *Statistics) printTitleStats() {
	if s.TitlesFetched == 0 && s.TitlesFailed == 0 {
		return
	}

	fmt.Printf("   Titles fetched:     %d", s.TitlesFetched)
	if s.TitlesFailed > 0 {
		fmt.Printf(" (%d failed)", s.TitlesFailed)
	}
	fmt.Println()
}

func (s *Statistics) printTagStats() {
	if s.TagsFetched == 0 && s.TagsFailed == 0 {
		return
	}

	fmt.Printf("   Tags fetched:       %d", s.TagsFetched)
	if s.TagsFailed > 0 {
		fmt.Printf(" (%d failed)", s.TagsFailed)
	}
	fmt.Println()
}

func (s *Statistics) Print() {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Println("════════════════════════════════════════════════════════")
	fmt.Println("                   PROCESSING STATISTICS")
	fmt.Println("════════════════════════════════════════════════════════")
	fmt.Println()

	s.printTiming()
	s.printActions()
	s.printUpdatesBreakdown()
	s.printURLProcessing()
	s.printHTTPResponses()
	s.printAPIOperations()

	fmt.Println("════════════════════════════════════════════════════════")
}
