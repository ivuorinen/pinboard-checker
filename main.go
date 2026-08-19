// Command pinboard-checker checks and repairs the bookmarks in a Pinboard
// account: it expands shortened URLs, follows redirects, fills in missing
// titles and tags, and removes links the server reports as gone.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/net/html"
)

const (
	// tokenName is the environment variable holding the Pinboard auth token.
	tokenName = "PINBOARD_API_TOKEN" //nolint:gosec // env var name, not a credential

	// bitlyTokenName holds a Bitly API token. Bitly's v4 /expand endpoint
	// rejects unauthenticated requests, so bit.ly URLs cannot be expanded
	// without one; they are left untouched rather than treated as broken.
	bitlyTokenName = "BITLY_ACCESS_TOKEN" //nolint:gosec // env var name, not a credential

	// defaultHTTPTimeout bounds every outbound request. Without a deadline a
	// host that accepts the connection and never answers holds a worker slot
	// for the life of the process, and enough of them deadlock the run.
	defaultHTTPTimeout = 15 * time.Second

	// defaultRateLimit matches Pinboard's documented one-call-per-three-seconds
	// budget. It widens on a 429 and narrows again on success; see
	// widenRateLimit and narrowRateLimit.
	defaultRateLimit = 3 * time.Second

	// maxRateLimit clamps the backoff. Without a ceiling five consecutive 429s
	// would push the interval to 96s and every later call would pay it.
	maxRateLimit = 60 * time.Second

	// maxRateLimitRetries bounds the 429 backoff loop so a permanently
	// throttled account fails with a named error instead of looping.
	maxRateLimitRetries = 5

	// maxWorkers caps -workers. Every worker holds an open connection, so
	// values beyond this exhaust file descriptors before they add throughput.
	maxWorkers = 100

	// maxTitleFetchBytes caps how much of a remote page is parsed as HTML.
	// html.Parse reads to EOF and retains the tree, so an unbounded response
	// from a bookmarked host would sit in memory once per worker. Any real
	// <head> fits far inside this.
	maxTitleFetchBytes = 1 << 20

	// maxPinboardTitleLen is Pinboard's documented limit for the `title` type.
	// Longer values are truncated or rejected server-side.
	maxPinboardTitleLen = 255

	// maxSuggestedTags leaves room for autoTagMarker inside the ten tags the
	// README promises.
	maxSuggestedTags = 9

	// autoTagMarker records that this tool chose the tags. Pinboard treats any
	// tag beginning with a period as a private tag.
	autoTagMarker = ".autoTagged"

	opURLValidation = "url-validation"
	opTitleFetch    = "title-fetch"
	opTagFetch      = "tag-fetch"

	paramURL  = "url"
	paramTags = "tags"
	valueYes  = "yes"

	// Statistics keys for the supported shorteners. Shared with
	// shortenerServices so the dispatch and the report cannot name them
	// differently.
	svcBitly   = "bitly"
	svcTinyURL = "tinyurl"
	svcIsGd    = "isgd"
)

// Service endpoints are variables so tests can point them at httptest servers
// instead of reaching the live internet.
var (
	pinboardAPIEndpoint = "https://api.pinboard.in/v1"
	isGdEndpoint        = "https://is.gd"
	bitlyEndpoint       = "https://api-ssl.bitly.com"
)

var (
	// errDeadLink marks a URL Pinboard should stop storing: the server answered
	// and reported the resource gone. Only this error may cause a deletion.
	errDeadLink = errors.New("dead link")

	// errUnverifiable marks a URL this tool could not check — a transport
	// failure, a timeout, or a scheme net/http cannot dial. Treating these as
	// dead links destroyed valid bookmarks (bookmarklets, mailto:, ftp:), so
	// they are counted and skipped instead.
	errUnverifiable = errors.New("unverifiable URL")

	// errNoBitlyToken is returned when bitlyTokenName is unset.
	errNoBitlyToken = errors.New("bit.ly expansion requires " + bitlyTokenName)

	// errRateLimited is returned once the 429 backoff loop gives up.
	errRateLimited = errors.New("pinboard rate limit exceeded")
)

// httpClient carries the deadline for every request. http.DefaultClient has a
// zero Timeout, which is why none of the package-level helpers are used here.
var httpClient = &http.Client{Timeout: defaultHTTPTimeout}

// redirectClient reports the first redirect instead of following it, so a
// bookmark can be rewritten to its destination. It shares httpClient's deadline.
var redirectClient = &http.Client{
	Timeout: defaultHTTPTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Bookmark mirrors one entry from posts/all. Time, Shared, and ToRead are
// carried purely so updateBookmark can hand them back: posts/add re-defaults
// every field it is not given, so omitting them reset creation dates, cleared
// the unread flag, and republished private bookmarks.
type Bookmark struct {
	Title  string `json:"description"`
	URL    string `json:"href"`
	Tags   string `json:"tags,omitempty"`
	Notes  string `json:"extended,omitempty"`
	Time   string `json:"time,omitempty"`
	Shared string `json:"shared,omitempty"`
	ToRead string `json:"toread,omitempty"`
}

type ActionType int

const (
	ActionNoChange ActionType = iota
	ActionUpdate
	ActionDelete
)

type BookmarkAction struct {
	Original Bookmark
	Action   ActionType
	NewURL   string
	NewTitle string
	NewTags  string
	Error    error
}

var (
	verbose      bool
	ciMode       bool
	skipTitles   bool
	skipAutoTags bool

	rateLimitInterval = defaultRateLimit
	rateLimiter       = time.NewTicker(defaultRateLimit)
	rateLimiterMux    sync.Mutex

	stats = newStatistics()
)

func main() {
	// A .env file is optional; a missing one is not an error.
	_ = godotenv.Load() //nolint:errcheck // .env file is optional

	// The token is deliberately NOT the flag default: flag.PrintDefaults renders
	// a non-empty default into the usage text, so -h (and any flag typo) printed
	// the live API token to the terminal and into CI logs.
	apiToken := flag.String("token", "", "Pinboard API token (default: $"+tokenName+")")
	dryRun := flag.Bool("dry-run", false, "Dry run mode")
	timeout := flag.Duration("timeout", defaultHTTPTimeout, "Per-request HTTP timeout")
	flag.BoolVar(&verbose, "verbose", false, "Verbose mode")
	flag.BoolVar(&ciMode, "ci", false, "CI mode (no progress bar or verbose output)")
	flag.BoolVar(&skipTitles, "skip-titles", false, "Skip fetching titles for bookmarks without them")
	flag.BoolVar(&skipAutoTags, "skip-auto-tags", false, "Skip auto-tagging bookmarks without tags")
	workers := flag.Int("workers", 10, "Number of concurrent workers (1-100)")
	flag.Parse()

	token := *apiToken
	if token == "" {
		token = os.Getenv(tokenName)
	}

	if code := validateFlags(token, *workers, *timeout); code != 0 {
		os.Exit(code)
	}

	setHTTPTimeout(*timeout)

	os.Exit(run(token, *workers, *dryRun))
}

// validateFlags returns a non-zero exit code for any invalid combination.
// -workers was previously unchecked: zero produced an unbuffered semaphore that
// deadlocked the dispatch loop, and a negative value panicked inside make.
func validateFlags(token string, workers int, timeout time.Duration) int {
	if token == "" {
		fmt.Fprintf(os.Stderr,
			"Error: API token is required (set %s or pass -token)\n", tokenName)

		return 1
	}
	if workers < 1 || workers > maxWorkers {
		fmt.Fprintf(os.Stderr,
			"Error: -workers must be between 1 and %d, got %d\n", maxWorkers, workers)

		return 1
	}
	if timeout <= 0 {
		fmt.Fprintf(os.Stderr, "Error: -timeout must be positive, got %s\n", timeout)

		return 1
	}

	return 0
}

func setHTTPTimeout(timeout time.Duration) {
	httpClient.Timeout = timeout
	redirectClient.Timeout = timeout
}

// run performs the pass and returns the process exit code. It is separate from
// main so os.Exit is reached from exactly one place, and so a failed fetch or a
// failed write produces a non-zero status: returning from main exited 0, which
// let a broken scheduled run report success.
func run(apiToken string, workers int, dryRun bool) int {
	stats.FetchBookmarksStart = time.Now()
	bookmarks, err := getBookmarks(apiToken)
	stats.FetchBookmarksDuration = time.Since(stats.FetchBookmarksStart)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error fetching bookmarks:", err)

		return 1
	}
	stats.TotalBookmarks = len(bookmarks)

	// Phase 1: process bookmarks in parallel and collect actions.
	stats.ProcessingStart = time.Now()
	actions := processBookmarksParallel(bookmarks, apiToken, workers)
	stats.ProcessingDuration = time.Since(stats.ProcessingStart)

	// Phase 2: apply the collected actions sequentially.
	stats.ApplicationStart = time.Now()
	applyBookmarkActions(actions, apiToken, dryRun)
	stats.ApplicationDuration = time.Since(stats.ApplicationStart)

	stats.TotalDuration = time.Since(stats.FetchBookmarksStart)

	if !ciMode {
		fmt.Println() // Blank line before stats
		stats.Print()
	}

	// Reported even under -ci: suppressing the progress bar must not suppress
	// the outcome, or a partly failed run is indistinguishable from a clean one.
	if failed := stats.ApplyErrorCount(); failed > 0 {
		fmt.Fprintf(os.Stderr, "%d change(s) failed to apply\n", failed)

		return 1
	}

	return 0
}

// closeBody closes a response body and reports a close failure on stderr.
// Close errors are diagnostic only, so they must not reach stdout where they
// would interleave with the statistics report.
func closeBody(body io.Closer) {
	if err := body.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "Error closing response body: %v\n", err)
	}
}

// waitForRateLimit blocks until the next request is allowed. The mutex is held
// across the receive so the budget is global rather than per-worker.
func waitForRateLimit() {
	rateLimiterMux.Lock()
	defer rateLimiterMux.Unlock()
	<-rateLimiter.C
}

// widenRateLimit doubles the request interval after a 429, up to maxRateLimit,
// and returns the new value. Pinboard's docs require clients to back off; a
// fixed-interval client keeps being rejected for the rest of the run.
func widenRateLimit() time.Duration {
	rateLimiterMux.Lock()
	defer rateLimiterMux.Unlock()

	rateLimitInterval = min(rateLimitInterval*2, maxRateLimit)
	rateLimiter.Reset(rateLimitInterval)

	return rateLimitInterval
}

// narrowRateLimit steps the interval back toward defaultRateLimit after a
// request succeeds, halving rather than resetting so the client stays cautious
// while Pinboard is near its limit. Without it a single transient 429 left the
// widened interval in place for the whole run, turning a momentary throttle
// into a permanent slowdown of up to maxRateLimit per call.
func narrowRateLimit() {
	rateLimiterMux.Lock()
	defer rateLimiterMux.Unlock()

	if rateLimitInterval <= defaultRateLimit {
		return
	}

	rateLimitInterval = max(rateLimitInterval/2, defaultRateLimit)
	rateLimiter.Reset(rateLimitInterval)
}

// sanitizeTransportError strips the request URL out of a transport failure.
// url.Error renders the full URL, and every Pinboard URL carries auth_token in
// its query string, so wrapping the raw error printed the account credential to
// stderr on any DNS failure, refused connection, or timeout.
func sanitizeTransportError(path string, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("pinboard %s request failed: %s: %w", path, urlErr.Op, urlErr.Err)
	}

	return fmt.Errorf("pinboard %s request failed: %w", path, err)
}

// pinboardGet issues a rate-limited Pinboard request, retrying with a widening
// interval on 429. Every Pinboard call goes through here so the backoff, the
// auth token, and the format=json parameter cannot be forgotten at one site:
// posts/suggest previously omitted format, and Pinboard defaults to XML, so
// auto-tagging failed to decode on every call.
//
// Errors never include the built URL, which carries the auth token; see
// sanitizeTransportError for the part of that guarantee net/http does not give
// for free.
func pinboardGet(path string, params url.Values, apiToken string) (*http.Response, error) {
	// Copy rather than mutate: url.Values is a map, so setting auth_token on
	// the caller's value would leave the credential in a map the caller may
	// later log as ordinary request parameters.
	query := make(url.Values, len(params)+2)
	maps.Copy(query, params)
	query.Set("auth_token", apiToken)
	query.Set("format", "json")

	apiURL := pinboardAPIEndpoint + path + "?" + query.Encode()

	for attempt := range maxRateLimitRetries {
		waitForRateLimit()

		resp, err := httpClient.Get(apiURL)
		if err != nil {
			return nil, sanitizeTransportError(path, err)
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			narrowRateLimit()

			return resp, nil
		}

		closeBody(resp.Body)
		interval := widenRateLimit()
		if verbose {
			fmt.Fprintf(os.Stderr,
				"Pinboard rate limited %s (attempt %d); interval now %s\n",
				path, attempt+1, interval)
		}
	}

	return nil, fmt.Errorf("%w: %s", errRateLimited, path)
}

// checkPinboardStatus turns a transport-level answer into a named error.
// getBookmarks previously skipped this and handed a 401 body straight to the
// JSON decoder, reporting a revoked token as "failed to decode bookmarks JSON".
func checkPinboardStatus(resp *http.Response, path string) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("pinboard rejected the API token (HTTP 401); check %s", tokenName)
	default:
		return fmt.Errorf("pinboard %s failed: status %d", path, resp.StatusCode)
	}
}

// decodePinboardResult checks the in-band result code Pinboard returns for
// writes. posts/add answers a rejected write with result_code "something went
// wrong" at HTTP 200, so a status-only check counted failures as successes.
func decodePinboardResult(resp *http.Response) error {
	var result struct {
		ResultCode string `json:"result_code"`
		Result     string `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode Pinboard write result: %w", err)
	}

	code := result.ResultCode
	if code == "" {
		code = result.Result
	}
	if code != "done" {
		return fmt.Errorf("pinboard rejected the write: %q", code)
	}

	return nil
}

// pinboardWrite performs a Pinboard write and reports whether it took effect.
// posts/add and posts/delete both answer a rejected write in the body at HTTP
// 200, so the status and the result code must both be checked; keeping that in
// one place is why neither caller can end up doing only half of it.
func pinboardWrite(path string, params url.Values, apiToken string) error {
	resp, err := pinboardGet(path, params, apiToken)
	if err != nil {
		return err
	}
	defer closeBody(resp.Body)

	if err := checkPinboardStatus(resp, path); err != nil {
		return err
	}

	return decodePinboardResult(resp)
}

// pinboardDecode performs a Pinboard read and decodes the JSON body into out.
// When statusOp is non-empty the HTTP status is recorded under that operation
// name for the statistics report.
func pinboardDecode(path, statusOp string, params url.Values, apiToken string, out any) error {
	resp, err := pinboardGet(path, params, apiToken)
	if err != nil {
		return err
	}
	defer closeBody(resp.Body)

	if statusOp != "" {
		stats.RecordHTTPStatus(statusOp, resp.StatusCode)
	}

	if err := checkPinboardStatus(resp, path); err != nil {
		return err
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("failed to decode %s response: %w", path, err)
	}

	return nil
}

func processBookmark(b Bookmark, apiToken string) BookmarkAction {
	if verbose {
		fmt.Printf("Processing: %s\n", b.URL)
	}

	if !isCheckableURL(b.URL) {
		return recordSkip(b, fmt.Errorf("%w: %s is not an http(s) URL", errUnverifiable, b.URL))
	}

	newURL, err := expandAndCheckURL(b.URL, verbose)
	if err != nil {
		// Only a server that answered "gone" justifies deleting a bookmark.
		// Timeouts, DNS failures, and undialable schemes are this tool's
		// limitation, not evidence the link is dead.
		if !errors.Is(err, errDeadLink) {
			return recordSkip(b, err)
		}

		action := BookmarkAction{Original: b, Action: ActionDelete, Error: err}
		stats.RecordAction(action)

		return action
	}

	// Reached only for a live URL: fetching a title or tags for a bookmark
	// already bound for deletion cost two requests and a rate-limiter slot for
	// results that were discarded.
	newTitle := fetchTitleIfNeeded(b, newURL)
	newTags := fetchTagsIfNeeded(b, apiToken, newURL)

	if shouldUpdateBookmark(b, newURL, newTitle, newTags) {
		action := BookmarkAction{
			Original: b,
			Action:   ActionUpdate,
			NewURL:   newURL,
			NewTitle: newTitle,
			NewTags:  newTags,
		}
		stats.RecordAction(action)

		return action
	}

	action := BookmarkAction{Original: b, Action: ActionNoChange}
	stats.RecordAction(action)

	return action
}

// recordSkip books a bookmark this tool could not verify. The bookmark is left
// exactly as it is; the reason is counted so the run reports what it skipped
// rather than staying silent about it.
func recordSkip(b Bookmark, reason error) BookmarkAction {
	if verbose {
		fmt.Fprintf(os.Stderr, "Skipping %s: %v\n", b.URL, reason)
	}

	action := BookmarkAction{Original: b, Action: ActionNoChange, Error: reason}
	stats.RecordSkipped()
	stats.RecordAction(action)

	return action
}

func shouldUpdateBookmark(b Bookmark, newURL, newTitle, newTags string) bool {
	urlChanged := newURL != "" && newURL != b.URL
	titleAdded := newTitle != ""
	tagsAdded := newTags != ""

	return urlChanged || titleAdded || tagsAdded
}

// targetURL prefers the rewritten URL when expansion produced one, so a title
// or tag lookup follows the bookmark to its new location.
func targetURL(original, rewritten string) string {
	if rewritten != "" {
		return rewritten
	}

	return original
}

func fetchTitleIfNeeded(b Bookmark, newURL string) string {
	if skipTitles || strings.TrimSpace(b.Title) != "" {
		return ""
	}

	fetchedTitle, err := getPageTitle(targetURL(b.URL, newURL))
	stats.RecordTitleFetch(err == nil && fetchedTitle != "")

	if err == nil && fetchedTitle != "" {
		if verbose {
			fmt.Printf("Fetched title: %s\n", fetchedTitle)
		}

		return fetchedTitle
	}

	return ""
}

func fetchTagsIfNeeded(b Bookmark, apiToken, newURL string) string {
	if skipAutoTags || strings.TrimSpace(b.Tags) != "" {
		return ""
	}

	fetchedTags, err := getSuggestedTags(apiToken, targetURL(b.URL, newURL))
	stats.RecordTagFetch(err == nil && fetchedTags != "")

	if err == nil && fetchedTags != "" {
		if verbose {
			fmt.Printf("Fetched tags: %s\n", fetchedTags)
		}

		return fetchedTags
	}

	return ""
}

func processBookmarksParallel(bookmarks []Bookmark, apiToken string, workers int) []BookmarkAction {
	var bar *progressbar.ProgressBar
	if !ciMode {
		bar = progressbar.NewOptions(len(bookmarks),
			progressbar.OptionSetDescription("Processing bookmarks..."),
			progressbar.OptionShowCount(),
			progressbar.OptionSetWidth(40),
		)
	}

	results := make(chan BookmarkAction, len(bookmarks))

	var wg sync.WaitGroup

	sem := make(chan struct{}, workers)

	for _, bookmark := range bookmarks {
		wg.Add(1)
		sem <- struct{}{}

		go func(b Bookmark) {
			defer wg.Done()
			defer func() { <-sem }()

			results <- processBookmark(b, apiToken)

			if bar != nil {
				_ = bar.Add(1) //nolint:errcheck // Progress bar errors are non-critical
			}
		}(bookmark)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	actions := make([]BookmarkAction, 0, len(bookmarks))
	for action := range results {
		actions = append(actions, action)
	}

	return actions
}

func countActionsToApply(actions []BookmarkAction) int {
	count := 0

	for _, action := range actions {
		if action.Action == ActionUpdate || action.Action == ActionDelete {
			count++
		}
	}

	return count
}

// reportApplyError prints a failed write when -verbose is set. Only the message
// is gated on the flag; the caller always records the failure, because counting
// it conditionally made a default run report nothing but successes.
func reportApplyError(operation, urlStr string, err error) {
	if verbose {
		fmt.Fprintf(os.Stderr, "Error %s %s: %v\n", operation, urlStr, err)
	}
}

// applyUpdateWrite posts the bookmark at its target URL and, when that URL
// changed, removes the stale entry. posts/add is keyed by URL, so without the
// delete a moved bookmark exists twice. The delete runs only after the add
// succeeded: interrupted the other way round, the bookmark would be lost.
func applyUpdateWrite(action BookmarkAction, apiToken string) {
	urlToUpdate := action.NewURL
	if urlToUpdate == "" {
		urlToUpdate = action.Original.URL
	}

	err := updateBookmark(apiToken, action.Original, urlToUpdate,
		action.NewTitle, action.NewTags)
	if err != nil {
		stats.RecordApplyError()
		reportApplyError("updating", action.Original.URL, err)

		return
	}

	stats.IncrementUpdates()

	if urlToUpdate == action.Original.URL {
		return
	}

	if err := deleteBookmark(apiToken, action.Original.URL); err != nil {
		stats.RecordApplyError()
		reportApplyError("removing old URL", action.Original.URL, err)
	}
}

func applyUpdateAction(action BookmarkAction, apiToken string, dryRun bool) {
	if !dryRun {
		applyUpdateWrite(action, apiToken)
	}

	if !verbose {
		return
	}

	if action.NewURL != "" && action.NewURL != action.Original.URL {
		fmt.Printf("Updated: %s -> %s\n", action.Original.URL, action.NewURL)
	}
	if action.NewTitle != "" {
		fmt.Printf("Added title: %s\n", action.NewTitle)
	}
	if action.NewTags != "" {
		fmt.Printf("Added tags: %s\n", action.NewTags)
	}
}

func applyDeleteAction(action BookmarkAction, apiToken string, dryRun bool) {
	if dryRun {
		if verbose {
			fmt.Printf("Would delete: %s (error: %v)\n", action.Original.URL, action.Error)
		}

		return
	}

	if err := deleteBookmark(apiToken, action.Original.URL); err != nil {
		stats.RecordApplyError()
		reportApplyError("deleting", action.Original.URL, err)

		return
	}

	stats.IncrementDeletions()

	if verbose {
		fmt.Printf("Deleted: %s\n", action.Original.URL)
	}
}

func applyBookmarkActions(actions []BookmarkAction, apiToken string, dryRun bool) {
	actionsToApply := countActionsToApply(actions)

	if actionsToApply == 0 {
		if verbose {
			fmt.Println("No changes to apply")
		}

		return
	}

	var bar *progressbar.ProgressBar
	if !ciMode {
		bar = progressbar.NewOptions(actionsToApply,
			progressbar.OptionSetDescription("Applying changes..."),
			progressbar.OptionShowCount(),
			progressbar.OptionSetWidth(40),
		)
	}

	for _, action := range actions {
		switch action.Action {
		case ActionUpdate:
			applyUpdateAction(action, apiToken, dryRun)
		case ActionDelete:
			applyDeleteAction(action, apiToken, dryRun)
		case ActionNoChange:
			continue
		}

		if bar != nil {
			_ = bar.Add(1) //nolint:errcheck // Progress bar errors are non-critical
		}
	}
}

// getBookmarks retrieves all bookmarks from the Pinboard API.
//
// Security note: Pinboard requires auth_token in the query string rather than a
// header. Tokens may therefore appear in intermediary logs; this is a property
// of Pinboard's API, not of this tool. HTTPS is enforced by the service.
func getBookmarks(apiToken string) ([]Bookmark, error) {
	const path = "/posts/all"

	var bookmarks []Bookmark
	if err := pinboardDecode(path, "", url.Values{}, apiToken, &bookmarks); err != nil {
		return nil, err
	}

	return bookmarks, nil
}

// updateBookmark writes a bookmark back to Pinboard.
//
// dt, shared, and toread are sent explicitly: posts/add re-defaults every field
// it is not given, so omitting them reset the creation date to now, cleared the
// unread flag, and republished bookmarks the user had made private.
func updateBookmark(apiToken string, b Bookmark, newURL, newTitle, newTags string) error {
	const path = "/posts/add"

	titleToUse := b.Title
	if newTitle != "" {
		titleToUse = newTitle
	}
	titleToUse = truncateRunes(titleToUse, maxPinboardTitleLen)

	tagsToUse := b.Tags
	if newTags != "" {
		tagsToUse = newTags
	}
	// Pinboard tags cannot contain whitespace; posts/all returns them
	// space-separated, posts/add accepts them comma-separated.
	tagsToUse = strings.ReplaceAll(tagsToUse, " ", ",")

	params := url.Values{
		paramURL:      {newURL},
		"description": {titleToUse},
		"extended":    {b.Notes},
		paramTags:     {tagsToUse},
		"replace":     {valueYes},
	}
	if b.Time != "" {
		params.Set("dt", b.Time)
	}
	if b.Shared != "" {
		params.Set("shared", b.Shared)
	}
	if b.ToRead != "" {
		params.Set("toread", b.ToRead)
	}

	return pinboardWrite(path, params, apiToken)
}

// deleteBookmark removes a bookmark from Pinboard.
func deleteBookmark(apiToken, urlStr string) error {
	return pinboardWrite("/posts/delete", url.Values{paramURL: {urlStr}}, apiToken)
}

// getPageTitle fetches a URL and extracts its <title>. It returns an empty
// string when no title is present; there is no fallback.
func getPageTitle(urlString string) (string, error) {
	resp, err := httpClient.Get(urlString)
	if err != nil {
		return "", fmt.Errorf("failed to fetch page for title extraction: %w", err)
	}
	defer closeBody(resp.Body)

	stats.RecordHTTPStatus(opTitleFetch, resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch page: status %d", resp.StatusCode)
	}
	if err := requireHTML(resp.Header.Get("Content-Type")); err != nil {
		return "", err
	}

	// LimitReader, not the whole body: html.Parse reads to EOF, so a large or
	// hostile response would be held in memory once per worker. A truncated
	// document still yields the <head>.
	doc, err := html.Parse(io.LimitReader(resp.Body, maxTitleFetchBytes))
	if err != nil {
		return "", fmt.Errorf("failed to parse HTML: %w", err)
	}

	title := strings.TrimSpace(extractTitleFromHTML(doc))

	return truncateRunes(title, maxPinboardTitleLen), nil
}

// requireHTML rejects responses that are not markup, so a video or an archive
// at a bookmarked URL is not parsed as a document.
func requireHTML(contentType string) error {
	if contentType == "" {
		return nil
	}

	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("unparseable Content-Type %q: %w", contentType, err)
	}
	if mediaType != "text/html" && mediaType != "application/xhtml+xml" {
		return fmt.Errorf("not an HTML page: %s", mediaType)
	}

	return nil
}

// truncateRunes cuts a string to at most limit runes. Cutting on bytes would
// split a multi-byte rune and produce invalid UTF-8 in the bookmark title.
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}

	return string(runes[:limit])
}

// extractTitleFromHTML returns the document title.
//
// It concatenates every text child rather than returning the first: x/net/html
// splits a title containing an entity reference into several sibling text
// nodes, so FirstChild alone truncated at the first entity. The namespace check
// skips <title> inside inline SVG, which is an icon label, not the page title.
func extractTitleFromHTML(node *html.Node) string {
	if node.Type == html.ElementNode && node.Data == "title" && node.Namespace == "" {
		var title strings.Builder
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.TextNode {
				title.WriteString(child.Data)
			}
		}

		return title.String()
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if title := extractTitleFromHTML(child); title != "" {
			return title
		}
	}

	return ""
}

// getSuggestedTags fetches Pinboard's tag suggestions for a URL and returns
// them comma-separated, with autoTagMarker appended. It returns an empty string
// when Pinboard suggested nothing: a bare marker made shouldUpdateBookmark
// rewrite bookmarks that gained no tags.
func getSuggestedTags(apiToken, urlStr string) (string, error) {
	const path = "/posts/suggest"

	// posts/suggest returns popular and recommended as separate array elements,
	// so every element is read rather than only the first.
	var suggestions []tagSuggestion

	params := url.Values{paramURL: {urlStr}}
	if err := pinboardDecode(path, opTagFetch, params, apiToken, &suggestions); err != nil {
		return "", err
	}

	return strings.Join(collectTagsFromSuggestions(suggestions), ","), nil
}

type tagSuggestion struct {
	Popular     []string `json:"popular"`
	Recommended []string `json:"recommended"`
}

// collectTagsFromSuggestions flattens Pinboard's suggestions into at most
// maxSuggestedTags entries plus autoTagMarker. It returns nil when there is
// nothing to add, so an untagged bookmark with no suggestions is left alone
// instead of being rewritten with only a marker.
func collectTagsFromSuggestions(suggestions []tagSuggestion) []string {
	seen := make(map[string]bool)
	tags := make([]string, 0, maxSuggestedTags+1)

	for _, suggestion := range suggestions {
		for _, group := range [][]string{suggestion.Popular, suggestion.Recommended} {
			for _, tag := range group {
				if tag == "" || seen[tag] || len(tags) >= maxSuggestedTags {
					continue
				}
				seen[tag] = true
				tags = append(tags, tag)
			}
		}
	}

	if len(tags) == 0 {
		return nil
	}

	return append(tags, autoTagMarker)
}

// isCheckableURL reports whether net/http can dial this URL. Pinboard also
// stores mailto:, ftp:, file:, and javascript: bookmarks, none of which
// http.Head can reach — treating that failure as a dead link deleted them.
func isCheckableURL(urlString string) bool {
	parsed, err := url.Parse(urlString)
	if err != nil {
		return false
	}

	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

// maxRedirectHops bounds redirect following. Resolving only the first hop left
// a bookmark pointing at an intermediate URL; following without a limit would
// loop on a redirect cycle.
const maxRedirectHops = 5

// expandAndCheckURL resolves a bookmark URL to its final form and confirms the
// result is reachable.
//
// The repair steps run before any liveness check, because they exist precisely
// to turn a URL that fails now into one that works: checking the input first
// made a URL broken only by its trailing parenthesis look like a dead link, and
// the bookmark was deleted instead of fixed.
func expandAndCheckURL(urlString string, verbose bool) (string, error) {
	steps := []urlStep{
		{name: "expanded URL", fn: expandURL},
		{name: "fixed URL", fn: fixParenthesesSuffix},
	}

	current := urlString
	for _, step := range steps {
		next, err := step.fn(current, verbose)
		if err != nil {
			return "", err
		}
		if next == current {
			continue
		}
		if !isCheckableURL(next) {
			return "", fmt.Errorf("%w: %s is not an http(s) URL: %q",
				errUnverifiable, step.name, next)
		}
		current = next
	}

	return followRedirects(current, verbose)
}

// followRedirects walks a redirect chain to its destination, checking liveness
// at each hop. The redirect probe and the liveness check share one request:
// running them as separate steps meant every bookmark was HEADed twice.
func followRedirects(urlString string, verbose bool) (string, error) {
	current := urlString

	for range maxRedirectHops {
		redirect, err := checkURL(current, "URL")
		if err != nil {
			return "", err
		}
		if redirect == "" {
			return current, nil
		}
		if !isCheckableURL(redirect) {
			return "", fmt.Errorf("%w: redirected URL is not an http(s) URL: %q",
				errUnverifiable, redirect)
		}

		stats.IncrementRedirects()

		if verbose {
			fmt.Printf("Redirected URL updated: '%s' -> '%s'\n", current, redirect)
		}

		current = redirect
	}

	// Chain longer than the hop limit: keep the last URL reached rather than
	// reporting a bookmark as broken because it redirects a lot.
	return current, nil
}

// urlStep is one transformation in the expand-and-check pipeline.
type urlStep struct {
	name string
	fn   func(string, bool) (string, error)
}

// checkURL performs one HEAD and reports both verdicts it carries: whether the
// URL is still live, and where it redirects. An empty redirect means the URL is
// final.
//
// Only a 4xx justifies errDeadLink. A transport failure yields errUnverifiable,
// because a timeout or a DNS hiccup is not evidence the bookmark is dead. 403
// and 405 are re-checked with GET first: many hosts refuse HEAD outright or
// serve it through bot protection, and deleting on that answer destroyed live
// bookmarks.
func checkURL(urlString, context string) (string, error) {
	resp, err := redirectClient.Head(urlString)
	if err != nil {
		return "", fmt.Errorf("%w: failed to check %s: %w", errUnverifiable, context, err)
	}
	defer closeBody(resp.Body)

	status := resp.StatusCode
	if status == http.StatusForbidden || status == http.StatusMethodNotAllowed {
		status = confirmWithGet(urlString, status)
	}

	stats.RecordHTTPStatus(opURLValidation, status)

	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return "", fmt.Errorf("%w: %s returns status %d", errDeadLink, context, status)
	}

	if !isRedirectStatus(status) {
		return "", nil
	}

	// resp.Location resolves a relative Location against the request URL;
	// returning the raw header stored values like "/new-path" as the bookmark.
	target, err := resp.Location()
	if err != nil {
		return "", nil //nolint:nilerr // a redirect with no usable Location is final
	}

	return target.String(), nil
}

// isRedirectStatus covers the full redirect set. Handling only 301 and 302
// missed most modern permanent redirects, which use 308.
func isRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

// validateURLAccessibility reports whether a URL is still live, discarding any
// redirect target. It exists for callers that only need the verdict.
func validateURLAccessibility(urlString, context string) error {
	_, err := checkURL(urlString, context)

	return err
}

// confirmWithGet re-requests a URL that refused HEAD, returning the GET status
// when one is obtained and the original status otherwise.
func confirmWithGet(urlString string, headStatus int) int {
	resp, err := httpClient.Get(urlString)
	if err != nil {
		return headStatus
	}
	defer closeBody(resp.Body)

	return resp.StatusCode
}

// fixParenthesesSuffix drops a trailing ")" when the URL works without it,
// repairing links copied out of Markdown.
func fixParenthesesSuffix(urlString string, verbose bool) (string, error) {
	if !strings.HasSuffix(urlString, ")") {
		return urlString, nil
	}

	updatedURL := strings.TrimSuffix(urlString, ")")
	if verbose {
		fmt.Printf("URL '%s' ends with ')', retrying without it.\n", urlString)
	}

	resp, err := httpClient.Head(updatedURL)
	if err != nil {
		// The trimmed variant is unreachable; keep the original rather than
		// reporting the bookmark as broken.
		return urlString, nil //nolint:nilerr // failure here means "no change"
	}
	defer closeBody(resp.Body)

	if resp.StatusCode == http.StatusOK {
		stats.IncrementParenthesesFixed()

		if verbose {
			fmt.Printf("URL updated: '%s' -> '%s'\n", urlString, updatedURL)
		}

		return updatedURL, nil
	}

	return urlString, nil
}

// expandURL resolves a known URL shortener to its destination.
//
// A failed expansion returns the original URL and no error: an unexpandable
// short link is not a dead link, and reporting one as an error routed the
// bookmark to deletion.
func expandURL(shortURL string, verbose bool) (string, error) {
	cleanedURL := strings.TrimPrefix(shortURL, "https://")
	cleanedURL = strings.TrimPrefix(cleanedURL, "http://")
	cleanedURL = strings.TrimPrefix(cleanedURL, "www.")

	if verbose {
		fmt.Printf("-> Prefix removed '%s'\n", cleanedURL)
	}

	service, found := shortenerFor(cleanedURL)
	if !found {
		return shortURL, nil
	}

	expanded, err := service.expand(shortURL)
	stats.RecordShortenerExpansion(service.key, err == nil)

	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "Could not expand %s via %s: %v\n",
				shortURL, service.display, err)
		}

		return shortURL, nil //nolint:nilerr // an unexpandable link is not a dead one
	}

	return expanded, nil
}

// shortenerFor returns the service handling a scheme-stripped URL.
func shortenerFor(cleanedURL string) (shortenerService, bool) {
	for _, service := range shortenerServices {
		if strings.HasPrefix(cleanedURL, service.prefix) {
			return service, true
		}
	}

	return shortenerService{}, false
}

// shortenerService ties a URL prefix to the stats key it is recorded under, the
// label the report prints, and the expander. Declared in one place so a service
// cannot be added to the dispatch and then silently omitted from the statistics
// block, which is what happens when the two lists are maintained separately.
type shortenerService struct {
	prefix  string
	key     string
	display string
	expand  func(string) (string, error)
}

// The expanders are called through closures so tests can swap the package-level
// variables below; reading them here directly would capture the originals at
// package-initialisation time.
var shortenerServices = []shortenerService{
	{"bit.ly/", svcBitly, "bit.ly", func(u string) (string, error) { return unshortenBitly(u) }},
	{"tinyurl.com/", svcTinyURL, "tinyurl",
		func(u string) (string, error) { return unshortenTinyURL(u) }},
	{"is.gd/", svcIsGd, "is.gd", func(u string) (string, error) { return unshortenIsGd(u) }},
}

// Shortener implementations are variables so tests can replace them without
// reaching the live services.
var (
	unshortenBitly   = unshortenBitlyImpl
	unshortenTinyURL = unshortenTinyURLImpl
	unshortenIsGd    = unshortenIsGdImpl
)

// unshortenBitlyImpl expands a bit.ly link through Bitly's v4 API.
//
// The endpoint requires a bearer token, takes bitlink_id (host and path, no
// scheme), and answers with long_url. The previous implementation sent no
// token, posted short_url, and decoded into an untagged LongURL field that
// could never match long_url — so it returned an empty URL with a nil error,
// and every bit.ly bookmark was then judged dead and deleted.
func unshortenBitlyImpl(shortURL string) (string, error) {
	token := os.Getenv(bitlyTokenName)
	if token == "" {
		return "", errNoBitlyToken
	}

	bitlinkID := strings.TrimPrefix(shortURL, "https://")
	bitlinkID = strings.TrimPrefix(bitlinkID, "http://")

	payload, err := json.Marshal(map[string]string{"bitlink_id": bitlinkID})
	if err != nil {
		return "", fmt.Errorf("failed to encode bit.ly request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost,
		bitlyEndpoint+"/v4/expand", strings.NewReader(string(payload)))
	if err != nil {
		return "", fmt.Errorf("failed to build bit.ly request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to expand bit.ly URL: %w", err)
	}
	defer closeBody(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bit.ly expansion failed: status %d", resp.StatusCode)
	}

	var body struct {
		LongURL string `json:"long_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("failed to decode bit.ly response: %w", err)
	}

	return validExpandedURL(body.LongURL)
}

// unshortenTinyURLImpl expands a tinyurl.com link by reading its redirect.
// The previous implementation called api-create.php, which creates short URLs
// rather than expanding them, and returned the raw response body as the URL.
func unshortenTinyURLImpl(shortURL string) (string, error) {
	resp, err := redirectClient.Head(shortURL)
	if err != nil {
		return "", fmt.Errorf("failed to expand tinyurl.com URL: %w", err)
	}
	defer closeBody(resp.Body)

	target, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("tinyurl.com gave no redirect (status %d): %w",
			resp.StatusCode, err)
	}

	return validExpandedURL(target.String())
}

// unshortenIsGdImpl expands an is.gd link through its lookup API, which answers
// with JSON. The previous implementation returned that JSON document itself as
// the bookmark's new URL.
func unshortenIsGdImpl(shortURL string) (string, error) {
	query := url.Values{"format": {"json"}, "shorturl": {shortURL}}

	resp, err := httpClient.Get(isGdEndpoint + "/forward.php?" + query.Encode())
	if err != nil {
		return "", fmt.Errorf("failed to expand is.gd URL: %w", err)
	}
	defer closeBody(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("is.gd lookup failed: status %d", resp.StatusCode)
	}

	var body struct {
		URL          string `json:"url"`
		ErrorCode    int    `json:"errorcode"`
		ErrorMessage string `json:"errormessage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("failed to decode is.gd response: %w", err)
	}
	if body.ErrorCode != 0 {
		return "", fmt.Errorf("is.gd error %d: %s", body.ErrorCode, body.ErrorMessage)
	}

	return validExpandedURL(body.URL)
}

// validExpandedURL rejects an expansion that is not a usable absolute URL, so a
// service's error page or empty field can never be mistaken for a destination
// and written back to the bookmark.
func validExpandedURL(candidate string) (string, error) {
	trimmed := strings.TrimSpace(candidate)
	if trimmed == "" {
		return "", errors.New("shortener returned an empty URL")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("shortener returned an unparseable URL %q: %w", trimmed, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("shortener returned a non-absolute URL %q", trimmed)
	}

	return trimmed, nil
}
