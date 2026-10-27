// Command pinboard-checker checks and repairs the bookmarks in a Pinboard
// account: it follows permanent redirects, fills in missing titles and tags,
// and removes links the server reports as gone.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const (
	// tokenName is the environment variable holding the Pinboard auth token.
	tokenName = "PINBOARD_API_TOKEN" //nolint:gosec // env var name, not a credential

	// defaultHTTPTimeout bounds every outbound request. Without a deadline a
	// host that accepts the connection and never answers holds a worker slot
	// for the life of the process, and enough of them deadlock the run.
	defaultHTTPTimeout = 15 * time.Second

	// defaultRateLimit matches Pinboard's documented one-call-per-three-seconds
	// budget. It widens on a 429 and narrows again on success; see
	// rateLimiter.widen and rateLimiter.narrow.
	defaultRateLimit = 3 * time.Second

	// maxRateLimit clamps the backoff. Without a ceiling five consecutive 429s
	// would push the interval to 96s and every later call would pay it.
	maxRateLimit = 60 * time.Second

	// maxRateLimitRetries bounds the 429 backoff loop so a permanently
	// throttled account fails with a named error instead of looping.
	maxRateLimitRetries = 5

	// defaultWorkers is the -workers default.
	defaultWorkers = 10

	// maxWorkers caps -workers. Every worker holds an open connection, so
	// values beyond this exhaust file descriptors before they add throughput.
	maxWorkers = 100

	// defaultPerHostInterval spaces requests to any one bookmarked host. Two
	// per second is slow enough not to look like a scrape and fast enough that
	// an account spread over many domains is unaffected, since distinct hosts
	// never wait on each other. Tune it with -host-interval.
	defaultPerHostInterval = 500 * time.Millisecond

	// maxRedirectHops bounds redirect following. Resolving only the first hop
	// left a bookmark pointing at an intermediate URL; following without a
	// limit would loop on a redirect cycle.
	maxRedirectHops = 5

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
)

// pinboardAPIEndpoint is the API newChecker points a run at. Every checker
// carries its own copy, which is what a test sets; this variable exists only so
// the cli tests that run a whole pass can aim it at an httptest server.
var pinboardAPIEndpoint = "https://api.pinboard.in/v1"

var (
	// errDeadLink marks a URL Pinboard should stop storing: the server answered
	// and reported the resource gone. Only this error may cause a deletion.
	errDeadLink = errors.New("dead link")

	// errUnverifiable marks a URL this tool could not check — a transport
	// failure, a timeout, or a scheme net/http cannot dial. Treating these as
	// dead links destroyed valid bookmarks (bookmarklets, mailto:, ftp:), so
	// they are counted and skipped instead.
	errUnverifiable = errors.New("unverifiable URL")

	// errRateLimited is returned once the 429 backoff loop gives up.
	errRateLimited = errors.New("pinboard rate limit exceeded")
)

// userAgentTransport names this tool on every outbound request. Go's default
// User-Agent is indistinguishable from any other script, so a host with abuse
// protection has nothing to allow-list and nothing to complain to; it answers
// 403 or 429 instead, which is precisely the answer this tool must not receive.
type userAgentTransport struct {
	base http.RoundTripper
}

func newUserAgentTransport() *userAgentTransport {
	return &userAgentTransport{base: http.DefaultTransport}
}

// userAgent builds the User-Agent once, on first use rather than at package
// initialisation, so it reads the version GoReleaser stamped. It was a field
// filled lazily inside RoundTrip, which net/http calls from every worker at
// once: the redirect client's first requests all came from the pool, and the
// unsynchronised write was a data race the race detector reported.
var userAgent = sync.OnceValue(func() string {
	return "pinboard-checker/" + versionString() +
		" (+https://github.com/ivuorinen/pinboard-checker)"
})

// RoundTrip sets the User-Agent unless the caller already chose one. The
// request is cloned first: RoundTrip must not modify the request it is given.
//
// The transport's error is returned unaltered. net/http inspects what RoundTrip
// returns to decide whether a request may be retried, and wraps it into the
// *url.Error that sanitizeTransportError later strips the token out of; adding
// a layer here changes both.
func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") != "" {
		return t.base.RoundTrip(req) //nolint:wrapcheck // see docstring
	}

	clone := req.Clone(req.Context())
	clone.Header.Set("User-Agent", userAgent())

	return t.base.RoundTrip(clone) //nolint:wrapcheck // see docstring
}

// rateLimiter spaces Pinboard API calls. The mutex is held across the wait so
// the budget is global rather than per-worker.
type rateLimiter struct {
	mu       sync.Mutex
	base     time.Duration
	interval time.Duration
	// next is the earliest start of the next call. Its zero value lets the
	// first call of a run go at once: Pinboard's budget is the spacing between
	// calls, and the time.Ticker this replaced made every run, and every CLI
	// test, wait a whole interval before its first one.
	next time.Time
}

func newRateLimiter(interval time.Duration) *rateLimiter {
	return &rateLimiter{base: interval, interval: interval}
}

// wait blocks until the next request is allowed and books the slot after it.
func (r *rateLimiter) wait() {
	r.mu.Lock()
	defer r.mu.Unlock()

	time.Sleep(time.Until(r.next))
	r.next = time.Now().Add(r.interval)
}

// widen doubles the request interval after a 429, up to maxRateLimit, and
// returns the new value. The retry waits the whole new interval from now.
// Pinboard's docs require clients to back off; a fixed-interval client keeps
// being rejected for the rest of the run.
func (r *rateLimiter) widen() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.interval = min(r.interval*2, maxRateLimit)
	r.next = time.Now().Add(r.interval)

	return r.interval
}

// narrow steps the interval back toward its base after a request succeeds,
// halving rather than resetting so the client stays cautious while Pinboard is
// near its limit. Without it a single transient 429 left the widened interval
// in place for the whole run, turning a momentary throttle into a permanent
// slowdown of up to maxRateLimit per call. The slot already booked stands; the
// shorter interval applies from the next call.
func (r *rateLimiter) narrow() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.interval = max(r.interval/2, r.base)
}

// hostThrottle spaces requests to any single bookmarked host. The Pinboard
// limiter does not cover these hosts: it guards one API, while a ten-worker
// pool checking forty bookmarks on one domain sends forty requests there in
// seconds. That reads as a scrape and is answered with 403 or 429 — answers
// this tool then had to interpret, having caused them itself. A zero interval
// disables it.
type hostThrottle struct {
	mu       sync.Mutex
	interval time.Duration
	next     map[string]time.Time
}

func newHostThrottle(interval time.Duration) *hostThrottle {
	return &hostThrottle{interval: interval, next: map[string]time.Time{}}
}

// wait blocks until this caller's reserved slot for the URL's host.
//
// The slot is reserved under the lock and waited for outside it, so requests to
// different hosts still run in parallel — unlike rateLimiter.wait, which holds
// its mutex across the wait precisely to make the Pinboard budget global.
func (h *hostThrottle) wait(rawURL string) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || h.interval <= 0 {
		return
	}

	h.mu.Lock()
	now := time.Now()
	slot := h.next[parsed.Host]
	if slot.Before(now) {
		slot = now
	}
	h.next[parsed.Host] = slot.Add(h.interval)
	h.mu.Unlock()

	time.Sleep(time.Until(slot))
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

// writeOutcome is what an update did, which is not always what it planned: a
// changed URL the account already holds is merged rather than written, and
// Pinboard may reject the write.
type writeOutcome int

const (
	outcomeUpdated writeOutcome = iota
	outcomeMerged
	outcomeFailed
)

// version is stamped by GoReleaser via -X main.version. It must stay a plain
// uninitialized string: cmd/link only patches a variable that is uninitialized
// or set to a constant, so a function initializer would make -X a silent no-op.
var version string

// osExit is a variable so main can be exercised without ending the test binary.
// It is the only reason main is not itself untestable.
var osExit = os.Exit

func main() {
	osExit(cli(os.Args[1:], os.Stdout, os.Stderr))
}

// checker holds one run: its configuration, its collaborators, and the state
// the run accumulates. cli builds one from the command line; every test builds
// its own.
//
// All of this used to be package globals. Tests had to swap them to set up a
// case, so 123 of them could not run in parallel, the suite never exercised the
// interleavings the worker pool produces, and cli's stderr argument reached the
// flag parser and nothing else.
type checker struct {
	out    io.Writer // the report and the -verbose narration
	errOut io.Writer // diagnostics, failure counts, and deletion records

	apiToken     string
	workers      int
	dryRun       bool
	verbose      bool
	ciMode       bool
	skipTitles   bool
	skipAutoTags bool

	pinboardEndpoint string

	// client follows redirects, guarded by checkRedirect; redirectClient
	// reports the first redirect instead of following it, so a bookmark can be
	// rewritten to its destination and no title is taken from a page the
	// bookmark does not point at.
	client         *http.Client
	redirectClient *http.Client

	// isPublicHost is a field so a test can bypass it: httptest serves on
	// 127.0.0.1, which the real check correctly rejects, so every redirect test
	// would otherwise be testing the guard rather than the redirect.
	isPublicHost func(string) bool

	// notFoundMasksPrivate lists hosts that answer 404, not 403, to an
	// unauthenticated request for a private resource. On them a 404 says
	// nothing about whether the page exists, so it is unverifiable rather than
	// dead. GitHub documents the policy ("GitHub uses a 404 Not Found response
	// instead of a 403 Forbidden response to avoid confirming the existence of
	// private repositories"), and every check here is unauthenticated, so
	// treating its 404 as dead deleted bookmarks of the user's own private
	// repositories. gist.github.com is absent on purpose: a secret gist opens
	// by URL, so its 404 does mean gone.
	notFoundMasksPrivate map[string]bool

	limiter *rateLimiter
	hosts   *hostThrottle
	stats   *Statistics

	// applyBar is the apply phase's progress bar: nil under -ci and before the
	// phase starts, finished once it ends. recordDeletion breaks its line
	// before writing a record.
	applyBar *progressBar

	// knownURLs holds every URL the account holds. run fills it from posts/all
	// and the sequential apply phase adds each URL it writes and removes each
	// one it deletes, so a second bookmark moving to the same new address is
	// merged rather than written over the first: posts/add with replace=yes
	// overwrites the whole entry, notes and tags included. Phase one never
	// reads it, so it needs no mutex.
	knownURLs map[string]bool
}

// newChecker returns a checker with the production defaults, writing the
// report to out and diagnostics to errOut.
func newChecker(out, errOut io.Writer) *checker {
	chk := &checker{
		out:                  out,
		errOut:               errOut,
		workers:              defaultWorkers,
		pinboardEndpoint:     pinboardAPIEndpoint,
		isPublicHost:         isPublicHostImpl,
		notFoundMasksPrivate: map[string]bool{"github.com": true},
		limiter:              newRateLimiter(defaultRateLimit),
		hosts:                newHostThrottle(defaultPerHostInterval),
		stats:                newStatistics(),
		knownURLs:            map[string]bool{},
	}
	chk.setHTTPTimeout(defaultHTTPTimeout)

	return chk
}

// setHTTPTimeout rebuilds both clients with the given per-request deadline.
// http.DefaultClient has a zero Timeout, which is why none of the package-level
// helpers are used anywhere here.
func (c *checker) setHTTPTimeout(timeout time.Duration) {
	c.client = &http.Client{
		Timeout:       timeout,
		Transport:     newUserAgentTransport(),
		CheckRedirect: c.checkRedirect,
	}
	c.redirectClient = &http.Client{
		Timeout:   timeout,
		Transport: newUserAgentTransport(),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// checkRedirect applies the private-address guard and the hop limit to every
// redirect c.client follows: the GET re-check and the parenthesis probe.
// Without it they followed up to ten redirects anywhere, so a bookmarked host
// could steer them at the user's own network with a 302 to 192.168.1.1. While
// the title fetch still used this client, that page's title was written to
// Pinboard.
func (c *checker) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirectHops {
		return http.ErrUseLastResponse
	}
	if !c.isPublicHost(req.URL.String()) {
		return fmt.Errorf("%w: redirect to non-public host %q", errUnverifiable, req.URL.Host)
	}

	return nil
}

// options holds the parsed command line. The pointers are the flag package's,
// filled by registerFlags at Parse time.
type options struct {
	token        *string
	dryRun       *bool
	timeout      *time.Duration
	hostInterval *time.Duration
	workers      *int
	showVersion  *bool
	verbose      *bool
	ciMode       *bool
	skipTitles   *bool
	skipAutoTags *bool
}

// registerFlags declares every flag on fs.
//
// It takes a FlagSet rather than using flag.CommandLine so the command line can
// be parsed more than once in one process — the global set panics on a second
// registration, which made main untestable. It is also the single list the
// README flag test checks against, so a flag cannot be added here and left out
// of the documentation.
func registerFlags(flagSet *flag.FlagSet) *options {
	return &options{
		// The token is deliberately NOT the flag default: PrintDefaults renders
		// a non-empty default into the usage text, so -h (and any flag typo)
		// printed the live API token to the terminal and into CI logs.
		token:  flagSet.String("token", "", "Pinboard API token (default: $"+tokenName+")"),
		dryRun: flagSet.Bool("dry-run", false, "Dry run mode"),
		timeout: flagSet.Duration("timeout", defaultHTTPTimeout,
			"Per-request HTTP timeout"),
		hostInterval: flagSet.Duration("host-interval", defaultPerHostInterval,
			"Minimum delay between requests to one bookmarked host (0 disables)"),
		workers: flagSet.Int("workers", defaultWorkers,
			"Number of concurrent workers (1-100)"),
		showVersion: flagSet.Bool("version", false, "Print version and exit"),
		verbose:     flagSet.Bool("verbose", false, "Verbose mode"),
		ciMode: flagSet.Bool("ci", false,
			"CI mode: no progress bars; the summary becomes one count line on stderr"),
		skipTitles: flagSet.Bool("skip-titles", false,
			"Skip fetching titles for bookmarks without them"),
		skipAutoTags: flagSet.Bool("skip-auto-tags", false,
			"Skip auto-tagging bookmarks without tags"),
	}
}

// cli parses args, applies them, and returns the process exit code.
//
// Split out of main so every branch above os.Exit is reachable from a test: the
// exit-code contract is the tool's CI-facing interface, and it previously had
// no coverage at all.
func cli(args []string, stdout, stderr io.Writer) int {
	chk, code := configure(args, stdout, stderr)
	if chk == nil {
		return code
	}

	return chk.run()
}

// configure turns the command line into a checker, or into the exit code that
// ends the process before any run: 0 for -h and -version, 2 for a malformed
// command line, 1 for an invalid combination or an unreadable .env.
func configure(args []string, stdout, stderr io.Writer) (*checker, int) {
	flagSet := flag.NewFlagSet("pinboard-checker", flag.ContinueOnError)
	flagSet.SetOutput(stderr)

	opts := registerFlags(flagSet)

	if err := flagSet.Parse(args); err != nil {
		// -h is a request, not a failure; anything else is a malformed command
		// line, which exits 2 as convention has it for usage errors.
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0
		}

		return nil, 2
	}

	if *opts.showVersion {
		fmt.Fprintln(stdout, versionString())

		return nil, 0
	}

	// -token wins, then the environment, then .env in the working directory.
	// The file is read only when it can matter, so -h, -version and an
	// explicit token never touch it.
	token := *opts.token
	if token == "" {
		token = os.Getenv(tokenName)
	}
	if token == "" {
		fromFile, err := tokenFromDotEnv(".env")
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)

			return nil, 1
		}
		token = fromFile
	}

	if code := validateFlags(stderr, token, *opts.workers, *opts.timeout,
		*opts.hostInterval); code != 0 {
		return nil, code
	}

	chk := newChecker(stdout, stderr)
	chk.apiToken = token
	chk.workers = *opts.workers
	chk.dryRun = *opts.dryRun
	chk.verbose = *opts.verbose
	chk.ciMode = *opts.ciMode
	chk.skipTitles = *opts.skipTitles
	chk.skipAutoTags = *opts.skipAutoTags
	chk.hosts = newHostThrottle(*opts.hostInterval)
	chk.setHTTPTimeout(*opts.timeout)

	return chk, 0
}

// tokenFromDotEnv returns the PINBOARD_API_TOKEN set in the .env file at path,
// or "" when the file does not exist or does not set it.
//
// It reads that one key and sets nothing in the process environment. The
// general dotenv loader this replaced (github.com/joho/godotenv) copied every
// key in the file into the environment, so a .env in the working directory
// could also set HTTPS_PROXY or SSL_CERT_FILE, which net/http and crypto/x509
// read afterwards.
//
// The syntax covered is what a token line needs: KEY=value, an "export "
// prefix, single or double quotes, # comment lines, blank lines, CRLF endings,
// a UTF-8 byte-order mark, and a " #" comment after an unquoted value. The
// last definition wins. Variable expansion and multi-line values are not
// supported; a value written that way is taken literally.
//
// A line without "=" is an error naming the line. Skipping it would turn a
// typo such as "PINBOARD_API_TOKEN user:abc" into an unset token, and the run
// would then blame the user for not setting one.
func tokenFromDotEnv(path string) (string, error) {
	content, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", path, err)
	}

	token := ""
	lines := strings.Split(strings.TrimPrefix(string(content), byteOrderMark), "\n")

	for number, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, found := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !found {
			return "", fmt.Errorf("%s line %d: expected KEY=value", path, number+1)
		}
		if strings.TrimSpace(key) == tokenName {
			token = dotEnvValue(strings.TrimSpace(value))
		}
	}

	return token, nil
}

// byteOrderMark is the UTF-8 BOM some Windows editors write at the start of a
// file; left in place it becomes part of the first key, and that key's line is
// silently ignored.
const byteOrderMark = string(rune(0xfeff))

// dotEnvValue unquotes one .env value. A quoted value is taken verbatim between
// its quotes, so a "#" inside one is part of the value; an unquoted value ends
// at a " #" comment. An unterminated quote is kept literally: a token sent with
// a stray quote is rejected by Pinboard with an error that names the token.
func dotEnvValue(raw string) string {
	if len(raw) >= 2 && (raw[0] == '"' || raw[0] == '\'') {
		if end := strings.IndexByte(raw[1:], raw[0]); end >= 0 {
			return raw[1 : end+1]
		}
	}

	if comment := strings.Index(raw, " #"); comment >= 0 {
		raw = raw[:comment]
	}

	return strings.TrimSpace(raw)
}

// versionString reports the release version. Only GoReleaser sets it; a
// `go install`ed or `go build`ed binary has an empty version, so fall back to
// the module version the toolchain stamps into the build info.
// readBuildInfo is a variable so the no-build-info fallback can be tested; a
// test binary always has build info, so "dev" was otherwise unreachable.
var readBuildInfo = debug.ReadBuildInfo

func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := readBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}

	return "dev"
}

// validateFlags returns a non-zero exit code for any invalid combination.
// -workers was previously unchecked: zero produced an unbuffered semaphore that
// deadlocked the dispatch loop, and a negative value panicked inside make.
//
// hostInterval may be zero — that switches the per-host throttle off, which is
// a legitimate choice for an account whose bookmarks are spread thin. Only a
// negative value is rejected, since it would silently mean the same as zero
// while reading like a setting.
func validateFlags(stderr io.Writer, token string, workers int,
	timeout, hostInterval time.Duration,
) int {
	if token == "" {
		fmt.Fprintf(stderr,
			"Error: API token is required (set %s or pass -token)\n", tokenName)

		return 1
	}
	if workers < 1 || workers > maxWorkers {
		fmt.Fprintf(stderr,
			"Error: -workers must be between 1 and %d, got %d\n", maxWorkers, workers)

		return 1
	}
	if timeout <= 0 {
		fmt.Fprintf(stderr, "Error: -timeout must be positive, got %s\n", timeout)

		return 1
	}
	if hostInterval < 0 {
		fmt.Fprintf(stderr,
			"Error: -host-interval must not be negative, got %s (use 0 to disable)\n",
			hostInterval)

		return 1
	}

	return 0
}

// run performs the pass and returns the process exit code. A failed fetch or a
// failed write produces a non-zero status: returning from main exited 0, which
// let a broken scheduled run report success.
func (c *checker) run() int {
	c.stats.FetchBookmarksStart = time.Now()
	bookmarks, err := c.getBookmarks()
	c.stats.FetchBookmarksDuration = time.Since(c.stats.FetchBookmarksStart)
	if err != nil {
		fmt.Fprintln(c.errOut, "Error fetching bookmarks:", err)

		return 1
	}
	c.stats.TotalBookmarks = len(bookmarks)

	c.knownURLs = make(map[string]bool, len(bookmarks))
	for _, bookmark := range bookmarks {
		c.knownURLs[bookmark.URL] = true
	}

	// Phase 1: process bookmarks in parallel and collect actions.
	c.stats.ProcessingStart = time.Now()
	actions := c.processBookmarksParallel(bookmarks)
	c.stats.ProcessingDuration = time.Since(c.stats.ProcessingStart)

	// Phase 2: apply the collected actions sequentially.
	c.stats.ApplicationStart = time.Now()
	c.applyBookmarkActions(actions)
	c.stats.ApplicationDuration = time.Since(c.stats.ApplicationStart)

	c.stats.TotalDuration = time.Since(c.stats.FetchBookmarksStart)

	if c.ciMode {
		// -ci drops the report, not the outcome: without this line a run that
		// deleted bookmarks and failed nothing printed nothing at all.
		fmt.Fprintln(c.errOut, c.stats.CISummary())
	} else {
		fmt.Fprintln(c.out) // Blank line before stats
		c.stats.Print(c.out)
	}

	// Reported even under -ci: suppressing the progress bar must not suppress
	// the outcome, or a partly failed run is indistinguishable from a clean one.
	if failed := c.stats.ApplyErrorCount(); failed > 0 {
		fmt.Fprintf(c.errOut, "%d change(s) failed to apply\n", failed)

		return 1
	}

	return 0
}

// closeBody closes a response body and reports a close failure on errOut.
// Close errors are diagnostic only, so they must not reach the report stream
// where they would interleave with the statistics.
func (c *checker) closeBody(body io.Closer) {
	if err := body.Close(); err != nil {
		fmt.Fprintf(c.errOut, "Error closing response body: %v\n", err)
	}
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
func (c *checker) pinboardGet(path string, params url.Values) (*http.Response, error) {
	// Copy rather than mutate: url.Values is a map, so setting auth_token on
	// the caller's value would leave the credential in a map the caller may
	// later log as ordinary request parameters.
	query := make(url.Values, len(params)+2)
	maps.Copy(query, params)
	query.Set("auth_token", c.apiToken)
	query.Set("format", "json")

	apiURL := c.pinboardEndpoint + path + "?" + query.Encode()

	for attempt := range maxRateLimitRetries {
		c.limiter.wait()

		resp, err := c.client.Get(apiURL)
		if err != nil {
			return nil, sanitizeTransportError(path, err)
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			c.limiter.narrow()

			return resp, nil
		}

		c.closeBody(resp.Body)
		interval := c.limiter.widen()
		if c.verbose {
			fmt.Fprintf(c.errOut,
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
func (c *checker) pinboardWrite(path string, params url.Values) error {
	resp, err := c.pinboardGet(path, params)
	if err != nil {
		return err
	}
	defer c.closeBody(resp.Body)

	if err := checkPinboardStatus(resp, path); err != nil {
		return err
	}

	return decodePinboardResult(resp)
}

// pinboardDecode performs a Pinboard read and decodes the JSON body into out.
// When statusOp is non-empty the HTTP status is recorded under that operation
// name for the statistics report.
func (c *checker) pinboardDecode(path, statusOp string, params url.Values, out any) error {
	resp, err := c.pinboardGet(path, params)
	if err != nil {
		return err
	}
	defer c.closeBody(resp.Body)

	if statusOp != "" {
		c.stats.RecordHTTPStatus(statusOp, resp.StatusCode)
	}

	if err := checkPinboardStatus(resp, path); err != nil {
		return err
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("failed to decode %s response: %w", path, err)
	}

	return nil
}

func (c *checker) processBookmark(b Bookmark) BookmarkAction {
	if c.verbose {
		fmt.Fprintf(c.out, "Processing: %s\n", b.URL)
	}

	if !isCheckableURL(b.URL) {
		return c.recordSkip(b, fmt.Errorf("%w: %s is not an http(s) URL", errUnverifiable, b.URL))
	}

	newURL, err := c.expandAndCheckURL(b.URL)
	if err != nil {
		// Only a server that answered "gone" justifies deleting a bookmark.
		// Timeouts, DNS failures, and undialable schemes are this tool's
		// limitation, not evidence the link is dead.
		if !errors.Is(err, errDeadLink) {
			return c.recordSkip(b, err)
		}

		action := BookmarkAction{Original: b, Action: ActionDelete, Error: err}
		c.stats.RecordAction(action)

		return action
	}

	// Reached only for a live URL: fetching a title or tags for a bookmark
	// already bound for deletion cost two requests and a rate-limiter slot for
	// results that were discarded.
	newTitle := c.fetchTitleIfNeeded(b, newURL)
	newTags := c.fetchTagsIfNeeded(b, newURL)

	if shouldUpdateBookmark(b, newURL, newTitle, newTags) {
		action := BookmarkAction{
			Original: b,
			Action:   ActionUpdate,
			NewURL:   newURL,
			NewTitle: newTitle,
			NewTags:  newTags,
		}
		c.stats.RecordAction(action)

		return action
	}

	action := BookmarkAction{Original: b, Action: ActionNoChange}
	c.stats.RecordAction(action)

	return action
}

// recordSkip books a bookmark this tool could not verify. The bookmark is left
// exactly as it is; the reason is counted so the run reports what it skipped
// rather than staying silent about it.
func (c *checker) recordSkip(b Bookmark, reason error) BookmarkAction {
	if c.verbose {
		fmt.Fprintf(c.errOut, "Skipping %s: %v\n", b.URL, reason)
	}

	action := BookmarkAction{Original: b, Action: ActionNoChange, Error: reason}
	c.stats.RecordSkipped()
	c.stats.RecordAction(action)

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

func (c *checker) fetchTitleIfNeeded(b Bookmark, newURL string) string {
	if c.skipTitles || strings.TrimSpace(b.Title) != "" {
		return ""
	}

	fetchedTitle, err := c.getPageTitle(targetURL(b.URL, newURL))
	c.stats.RecordTitleFetch(fetchedTitle != "", err)

	if err == nil && fetchedTitle != "" {
		if c.verbose {
			fmt.Fprintf(c.out, "Fetched title: %s\n", fetchedTitle)
		}

		return fetchedTitle
	}

	return ""
}

func (c *checker) fetchTagsIfNeeded(b Bookmark, newURL string) string {
	if c.skipAutoTags || strings.TrimSpace(b.Tags) != "" {
		return ""
	}

	fetchedTags, err := c.getSuggestedTags(targetURL(b.URL, newURL))
	c.stats.RecordTagFetch(fetchedTags != "", err)

	if err == nil && fetchedTags != "" {
		if c.verbose {
			fmt.Fprintf(c.out, "Fetched tags: %s\n", fetchedTags)
		}

		return fetchedTags
	}

	return ""
}

// progressBarWidth is the number of cells in the bar itself.
const progressBarWidth = 40

// progressReportInterval spaces the progress lines -verbose prints in place of
// the bar.
const progressReportInterval = 15 * time.Second

// progressBar draws one line that redraws itself with "\r":
//
//	Processing bookmarks...  42% |████████████████                        | (42/100) [3s:4s]
//
// The bracket holds the elapsed time and the estimate of what remains. Under
// -verbose it prints a plain line every progressReportInterval instead; see
// newProgressBar.
//
// It replaces github.com/schollz/progressbar, which linked five of the
// binary's six third-party modules for the four options used here. It draws a
// fixed width and assumes a terminal; -ci is how a non-interactive run turns
// it off.
//
// increment is safe for concurrent use, because the worker pool reports from
// every goroutine. A nil *progressBar ignores increment, so the call sites need
// no -ci check.
type progressBar struct {
	mu          sync.Mutex
	out         io.Writer
	description string
	total       int
	done        int
	start       time.Time
	// longest is the widest frame drawn so far, in runes. Each frame is padded
	// to it, so a frame that came out shorter than the one before — the
	// estimate shrinking from "1m5s" to "59s" — leaves no stale tail behind.
	longest int
	// every, when set, replaces the frame with a progress line printed at
	// most that often; reportedAt is the elapsed time of the last one.
	every      time.Duration
	reportedAt time.Duration
}

// newProgressBar returns a progress bar on the report stream, or nil under -ci
// and when there is nothing to count.
//
// Under -verbose it reports a line of its own every progressReportInterval
// instead of redrawing a frame. The narration is written between frames, and
// each of its lines started right after the frame drawn before it; the
// periodic line still says how far the run is and how long it has left.
func (c *checker) newProgressBar(total int, description string) *progressBar {
	if c.ciMode || total <= 0 {
		return nil
	}

	bar := &progressBar{out: c.out, description: description, total: total, start: time.Now()}
	if c.verbose {
		bar.every = progressReportInterval
	}

	return bar
}

// increment records one finished item and redraws the bar. The last item ends
// the line, so the next bar starts on a line of its own rather than drawing
// over this one.
func (p *progressBar) increment() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.done++
	elapsed := time.Since(p.start)

	if p.every > 0 {
		p.report(elapsed)

		return
	}

	frame := p.line(elapsed)
	p.longest = max(p.longest, utf8.RuneCountInString(frame))
	fmt.Fprintf(p.out, "\r%-*s", p.longest, frame)

	if p.done >= p.total {
		fmt.Fprintln(p.out)
	}
}

// report prints the progress as a line of its own once every has passed since
// the last report, or since the start. It runs when an item finishes, so a
// line can come up to one item's duration late; a ticker would be punctual
// but needs a goroutine stopped at the end of every phase.
func (p *progressBar) report(elapsed time.Duration) {
	if elapsed-p.reportedAt < p.every {
		return
	}

	p.reportedAt = elapsed
	fmt.Fprintf(p.out, "%s %d/%d (%d%%), %s elapsed, %s left\n",
		p.description, p.done, p.total, p.done*100/p.total,
		elapsed.Round(time.Second), p.remaining(elapsed))
}

// breakLine ends the line a frame is drawn on, so a line written to another
// stream starts at the left margin instead of after the frame. A frame is on
// screen only in bar mode, between the first increment and the last, which
// already ends its line. The next increment draws below whatever was written.
func (p *progressBar) breakLine() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.every == 0 && p.done > 0 && p.done < p.total {
		fmt.Fprintln(p.out)
	}
}

// remaining estimates the time still to go as the elapsed time scaled by the
// items left, which is what the progressbar module showed by default; "?"
// until the first item finishes.
func (p *progressBar) remaining(elapsed time.Duration) string {
	if p.done == 0 {
		return "?"
	}

	estimate := float64(elapsed) * float64(p.total-p.done) / float64(p.done)

	return time.Duration(estimate).Round(time.Second).String()
}

// line renders the bar for the current count after elapsed.
func (p *progressBar) line(elapsed time.Duration) string {
	filled := min(p.done*progressBarWidth/p.total, progressBarWidth)

	return fmt.Sprintf("%s %3d%% |%s%s| (%d/%d) [%s:%s]",
		p.description, p.done*100/p.total,
		strings.Repeat("█", filled), strings.Repeat(" ", progressBarWidth-filled),
		p.done, p.total, elapsed.Round(time.Second), p.remaining(elapsed))
}

func (c *checker) processBookmarksParallel(bookmarks []Bookmark) []BookmarkAction {
	bar := c.newProgressBar(len(bookmarks), "Processing bookmarks...")

	results := make(chan BookmarkAction, len(bookmarks))

	var wg sync.WaitGroup

	sem := make(chan struct{}, c.workers)

	for _, bookmark := range interleaveHosts(bookmarks) {
		sem <- struct{}{}

		wg.Go(func() {
			defer func() { <-sem }()

			results <- c.processBookmark(bookmark)
			bar.increment()
		})
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

// interleaveHosts reorders bookmarks round-robin across their hosts, keeping
// each host's bookmarks in their original order.
//
// hostThrottle.wait sleeps while holding a worker slot. posts/all is ordered by
// date, so a day's reading on one site arrives as a run of same-host
// bookmarks; every worker then reserved a later slot on that host and slept,
// and bookmarks on other hosts queued behind them. Interleaving lets the pool
// spend that wait on other hosts.
func interleaveHosts(bookmarks []Bookmark) []Bookmark {
	var hosts []string

	byHost := map[string][]Bookmark{}

	for _, bookmark := range bookmarks {
		host := ""
		if parsed, err := url.Parse(bookmark.URL); err == nil {
			host = parsed.Host
		}
		if _, seen := byHost[host]; !seen {
			hosts = append(hosts, host)
		}
		byHost[host] = append(byHost[host], bookmark)
	}

	ordered := make([]Bookmark, 0, len(bookmarks))
	for len(ordered) < len(bookmarks) {
		for _, host := range hosts {
			if queue := byHost[host]; len(queue) > 0 {
				ordered = append(ordered, queue[0])
				byHost[host] = queue[1:]
			}
		}
	}

	return ordered
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
func (c *checker) reportApplyError(operation, urlStr string, err error) {
	if c.verbose {
		fmt.Fprintf(c.errOut, "Error %s %s: %v\n", operation, urlStr, err)
	}
}

// updateTarget is the URL an update writes to.
func updateTarget(action BookmarkAction) string {
	if action.NewURL != "" {
		return action.NewURL
	}

	return action.Original.URL
}

// applyUpdateWrite posts the bookmark at its target URL and, when that URL
// changed, removes the stale entry. posts/add is keyed by URL, so without the
// delete a moved bookmark exists twice. The delete runs only after the add
// succeeded: interrupted the other way round, the bookmark would be lost.
//
// A target the account already holds is merged instead of written: see
// mergeIntoExisting.
func (c *checker) applyUpdateWrite(action BookmarkAction) writeOutcome {
	target := updateTarget(action)

	if target != action.Original.URL && c.knownURLs[target] {
		if !c.mergeIntoExisting(action) {
			return outcomeFailed
		}

		return outcomeMerged
	}

	err := c.updateBookmark(action.Original, target, action.NewTitle, action.NewTags)
	if err != nil {
		c.stats.RecordApplyError()
		c.reportApplyError("updating", action.Original.URL, err)

		return outcomeFailed
	}

	c.stats.IncrementUpdates()

	if target == action.Original.URL {
		return outcomeUpdated
	}

	c.knownURLs[target] = true

	if err := c.deleteBookmark(action.Original.URL); err != nil {
		c.stats.RecordApplyError()
		c.reportApplyError("removing old URL", action.Original.URL, err)

		return outcomeUpdated
	}

	c.recordDeletion("moved to "+target, action.Original)

	return outcomeUpdated
}

// mergeIntoExisting drops the stale bookmark when its new URL is one the
// account already holds, leaving the existing entry untouched, and reports
// whether the delete went through.
//
// The alternative is what this code used to do: post the moved bookmark's
// fields at the destination with replace=yes, which overwrites the whole entry
// there. Two bookmarks of the same page are ordinary — an http:// one that
// redirects to its https:// twin, two shortener links to one article — and
// overwriting silently replaced the destination's hand-written notes and tags
// with the source's empty ones, then deleted the only copy they survived in.
// The destination is the entry the user curated; the mover is the duplicate.
func (c *checker) mergeIntoExisting(action BookmarkAction) bool {
	if err := c.deleteBookmark(action.Original.URL); err != nil {
		c.stats.RecordApplyError()
		c.reportApplyError("removing merged duplicate", action.Original.URL, err)

		return false
	}

	c.stats.RecordMerged()
	c.recordDeletion("merged into "+updateTarget(action), action.Original)

	return true
}

// applyUpdateAction applies one update, or under -dry-run works out what it
// would do, and narrates the outcome under -verbose.
func (c *checker) applyUpdateAction(action BookmarkAction) {
	var outcome writeOutcome

	target := updateTarget(action)

	switch {
	case !c.dryRun:
		outcome = c.applyUpdateWrite(action)
	case target != action.Original.URL && c.knownURLs[target]:
		outcome = outcomeMerged
		delete(c.knownURLs, action.Original.URL)
	default:
		outcome = outcomeUpdated
		// Mirror the real run, which forgets each URL it deletes and records
		// each one it writes, so a second bookmark moving to the same address
		// previews as the merge it is, and one moving onto a deleted address
		// previews as a write. Forget first: an in-place update's target is its
		// own URL, which must stay known.
		delete(c.knownURLs, action.Original.URL)
		c.knownURLs[target] = true
	}

	if c.verbose {
		c.narrateUpdate(action, target, outcome)
	}
}

// narrateUpdate prints what an update did, or under -dry-run what it would do.
// It reports the outcome rather than the plan: printing "Updated:" after every
// write claimed success for writes Pinboard rejected and for merges, which
// delete the bookmark instead, and a dry run previewed a merge as an update.
func (c *checker) narrateUpdate(action BookmarkAction, target string, outcome writeOutcome) {
	moved := target != action.Original.URL

	switch outcome {
	case outcomeFailed:
		return
	case outcomeMerged:
		if c.dryRun {
			fmt.Fprintf(c.out, "Would merge: %s already bookmarked as %s; would remove "+
				"the duplicate\n", action.Original.URL, target)
		} else {
			fmt.Fprintf(c.out, "Merged: %s already bookmarked as %s; removed the duplicate\n",
				action.Original.URL, target)
		}

		return
	case outcomeUpdated:
	}

	verbs := [3]string{"Updated", "Added title", "Added tags"}
	if c.dryRun {
		verbs = [3]string{"Would update", "Would add title", "Would add tags"}
	}

	if moved {
		fmt.Fprintf(c.out, "%s: %s -> %s\n", verbs[0], action.Original.URL, target)
	}
	if action.NewTitle != "" {
		fmt.Fprintf(c.out, "%s: %s\n", verbs[1], action.NewTitle)
	}
	if action.NewTags != "" {
		fmt.Fprintf(c.out, "%s: %s\n", verbs[2], action.NewTags)
	}
}

func (c *checker) applyDeleteAction(action BookmarkAction) {
	if c.dryRun {
		// The real run forgets the URL in deleteBookmark; see applyUpdateAction.
		delete(c.knownURLs, action.Original.URL)

		if c.verbose {
			fmt.Fprintf(c.out, "Would delete: %s (error: %v)\n", action.Original.URL, action.Error)
		}

		return
	}

	if err := c.deleteBookmark(action.Original.URL); err != nil {
		c.stats.RecordApplyError()
		c.reportApplyError("deleting", action.Original.URL, err)

		return
	}

	c.stats.IncrementDeletions()
	c.recordDeletion("dead link", action.Original)

	if c.verbose {
		fmt.Fprintf(c.out, "Deleted: %s\n", action.Original.URL)
	}
}

func (c *checker) applyBookmarkActions(actions []BookmarkAction) {
	actionsToApply := countActionsToApply(actions)

	if actionsToApply == 0 {
		if c.verbose {
			fmt.Fprintln(c.out, "No changes to apply")
		}

		return
	}

	c.applyBar = c.newProgressBar(actionsToApply, "Applying changes...")

	for _, action := range actions {
		switch action.Action {
		case ActionUpdate:
			c.applyUpdateAction(action)
		case ActionDelete:
			c.applyDeleteAction(action)
		case ActionNoChange:
			continue
		}

		c.applyBar.increment()
	}
}

// getBookmarks retrieves all bookmarks from the Pinboard API.
//
// Security note: Pinboard requires auth_token in the query string rather than a
// header. Tokens may therefore appear in intermediary logs; this is a property
// of Pinboard's API, not of this tool. HTTPS is enforced by the service.
func (c *checker) getBookmarks() ([]Bookmark, error) {
	var bookmarks []Bookmark
	if err := c.pinboardDecode("/posts/all", "", url.Values{}, &bookmarks); err != nil {
		return nil, err
	}

	return bookmarks, nil
}

// updateBookmark writes a bookmark back to Pinboard at newURL, replacing its
// title and tags only where new ones were found.
func (c *checker) updateBookmark(b Bookmark, newURL, newTitle, newTags string) error {
	updated := b
	updated.URL = newURL
	if newTitle != "" {
		updated.Title = newTitle
	}
	if newTags != "" {
		updated.Tags = newTags
	}

	return c.pinboardWrite("/posts/add", addParams(updated))
}

// addParams returns the posts/add parameters that write b exactly as given. It
// is shared by updateBookmark and recordDeletion so the record of a deleted
// bookmark is precisely the call that would recreate it.
//
// dt, shared, and toread are sent explicitly: posts/add re-defaults every field
// it is not given, so omitting them reset the creation date to now, cleared the
// unread flag, and republished bookmarks the user had made private.
func addParams(b Bookmark) url.Values {
	params := url.Values{
		paramURL:      {b.URL},
		"description": {truncateRunes(b.Title, maxPinboardTitleLen)},
		"extended":    {b.Notes},
		// Pinboard tags cannot contain whitespace; posts/all returns them
		// space-separated, posts/add accepts them comma-separated.
		paramTags: {strings.ReplaceAll(b.Tags, " ", ",")},
		"replace": {valueYes},
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

	return params
}

// recordDeletion writes a bookmark Pinboard has just deleted to errOut, at
// every verbosity and under -ci. Pinboard has no trash, and a run used to
// report deletions only under -verbose, so a wrongful one could be neither
// noticed nor undone. The line carries the posts/add query that restores the
// bookmark — URL, title, notes, tags, date, privacy — and therefore the
// bookmark's private fields: a log that captures stderr holds them too.
//
// The apply bar gives up its line first. On a terminal stdout and stderr share
// one screen, and the record otherwise started right after the bar's frame, so
// restoring a bookmark meant cutting a frame off the front of the line.
func (c *checker) recordDeletion(reason string, b Bookmark) {
	c.applyBar.breakLine()
	fmt.Fprintf(c.errOut, "deleted (%s): %s\n", reason, addParams(b).Encode())
}

// deleteBookmark removes a bookmark from Pinboard, and its URL from knownURLs.
// Every delete of the apply phase comes through here. Left in the map, a
// deleted URL turned a later bookmark moving onto it into a merge with an entry
// that no longer existed, and that bookmark was deleted too.
func (c *checker) deleteBookmark(urlStr string) error {
	if err := c.pinboardWrite("/posts/delete", url.Values{paramURL: {urlStr}}); err != nil {
		return err
	}

	delete(c.knownURLs, urlStr)

	return nil
}

// getPageTitle fetches a URL and extracts its <title>. It returns an empty
// string when no title is present; there is no fallback.
//
// It does not follow redirects. followRedirects has already moved the bookmark
// along every permanent one, so a redirect here leads to a page the bookmark
// does not point at. Following it titled an untitled bookmark behind a login
// wall "Sign in to …" while its URL was correctly kept. A redirect answer is a
// non-200 like any other, and no title is taken from it.
func (c *checker) getPageTitle(urlString string) (string, error) {
	c.hosts.wait(urlString)

	resp, err := c.redirectClient.Get(urlString)
	if err != nil {
		return "", fmt.Errorf("failed to fetch page for title extraction: %w", err)
	}
	defer c.closeBody(resp.Body)

	c.stats.RecordHTTPStatus(opTitleFetch, resp.StatusCode)

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
func (c *checker) getSuggestedTags(urlStr string) (string, error) {
	// posts/suggest returns popular and recommended as separate array elements,
	// so every element is read rather than only the first.
	var suggestions []tagSuggestion

	params := url.Values{paramURL: {urlStr}}
	if err := c.pinboardDecode("/posts/suggest", opTagFetch, params, &suggestions); err != nil {
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

// isPublicHostImpl reports whether every address a URL's host resolves to is
// publicly routable.
//
// A redirect target is chosen by a remote party, not by the user. Without this
// check a bookmarked host could redirect the tool to http://169.254.169.254/ or
// a service on localhost, and the tool would then rewrite the bookmark to that
// address and post the internal page's title to Pinboard — a third-party
// service.
//
// The ceiling: this resolves before dialing, so a host that answers with a
// public address here and a private one to the dialer still gets through.
// Closing that needs a DialContext hook on the transport, which is worth adding
// only if this proves insufficient. A host that will not resolve is treated as
// non-public, because an unresolvable target cannot be verified either way.
func isPublicHostImpl(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	addrs, err := net.LookupIP(parsed.Hostname())
	if err != nil || len(addrs) == 0 {
		return false
	}

	for _, ip := range addrs {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return false
		}
	}

	return true
}

// expandAndCheckURL repairs a bookmark URL, follows it to its final form, and
// confirms the result is reachable.
//
// The parenthesis repair runs before any liveness check, because it exists
// precisely to turn a URL that fails now into one that works: checking the
// input first made a URL broken only by its trailing parenthesis look like a
// dead link, and the bookmark was deleted instead of fixed.
//
// Short links need no step of their own. bit.ly, tinyurl.com and is.gd answer
// with a 301 to the destination, which followRedirects walks like any other
// redirect; the per-service API expanders that used to run here produced the
// same result through a second code path and a second credential.
func (c *checker) expandAndCheckURL(urlString string) (string, error) {
	return c.followRedirects(c.fixParenthesesSuffix(urlString))
}

// followRedirects walks a redirect chain to its destination, checking liveness
// at each hop. The redirect probe and the liveness check share one request:
// running them as separate steps meant every bookmark was HEADed twice.
//
// The whole chain is walked, because the liveness verdict belongs to the final
// answer, but the URL written back advances only while every hop so far was
// permanent. Persisting a 302 rewrote each session-gated bookmark to its login
// page; see isPermanentRedirect.
//
// A chain that has not ended within maxRedirectHops is unverifiable: the last
// Location was never requested. Returning it rewrote bookmarks to addresses
// nobody had checked, and a permanent A↔B cycle wrote each bookmark onto the
// other, so the apply phase merged one into the other and deleted both.
func (c *checker) followRedirects(urlString string) (string, error) {
	current := urlString
	rewrite := urlString
	permanent := true

	for range maxRedirectHops {
		redirect, movedForGood, err := c.checkURL(current, "URL")
		if err != nil {
			return "", err
		}
		if redirect == "" {
			return rewrite, nil
		}
		if err := c.followableRedirect(redirect); err != nil {
			return "", err
		}

		c.stats.IncrementRedirects()

		permanent = permanent && movedForGood
		if permanent {
			rewrite = redirect
		}
		c.narrateRedirect(current, redirect, permanent)

		current = redirect
	}

	return "", fmt.Errorf("%w: redirect chain longer than %d hops", errUnverifiable, maxRedirectHops)
}

// followableRedirect refuses a redirect target this tool must not follow: a
// scheme net/http cannot dial, or a host in private address space, which a
// bookmarked host could otherwise steer the tool at. Either way the bookmark is
// unverifiable, never dead.
func (c *checker) followableRedirect(redirect string) error {
	if !isCheckableURL(redirect) {
		return fmt.Errorf("%w: redirected URL is not an http(s) URL: %q", errUnverifiable, redirect)
	}
	if !c.isPublicHost(redirect) {
		return fmt.Errorf("%w: redirected URL is not a public host: %q", errUnverifiable, redirect)
	}

	return nil
}

// narrateRedirect reports one hop under -verbose, saying whether the bookmark
// follows it or keeps its URL.
func (c *checker) narrateRedirect(current, redirect string, rewritten bool) {
	if !c.verbose {
		return
	}

	if rewritten {
		fmt.Fprintf(c.out, "Redirected URL updated: '%s' -> '%s'\n", current, redirect)

		return
	}

	fmt.Fprintf(c.out, "Temporary redirect followed, URL kept: '%s' -> '%s'\n", current, redirect)
}

// checkURL performs one HEAD and reports the three verdicts it carries: whether
// the URL is still live, where it redirects, and whether that redirect is
// permanent. An empty redirect means the URL is final.
//
// Only 404 and 410 justify errDeadLink; see isDeadLinkStatus. Every other 4xx,
// and any transport failure, yields errUnverifiable, because being refused is
// not evidence the bookmark is gone. 403 and 405 are re-checked with GET first:
// many hosts refuse HEAD outright or serve it through bot protection.
func (c *checker) checkURL(urlString, context string) (string, bool, error) {
	c.hosts.wait(urlString)

	resp, err := c.redirectClient.Head(urlString)
	if err != nil {
		return "", false, fmt.Errorf("%w: failed to check %s: %w",
			errUnverifiable, context, err)
	}
	defer c.closeBody(resp.Body)

	status := resp.StatusCode
	if status == http.StatusForbidden || status == http.StatusMethodNotAllowed {
		status = c.confirmWithGet(urlString, status)
	}

	c.stats.RecordHTTPStatus(opURLValidation, status)

	host := strings.ToLower(resp.Request.URL.Hostname())
	if status == http.StatusNotFound && c.notFoundMasksPrivate[host] {
		return "", false, fmt.Errorf("%w: %s returns 404 from %s, which answers 404 "+
			"for private resources", errUnverifiable, context, host)
	}
	if isDeadLinkStatus(status) {
		return "", false, fmt.Errorf("%w: %s returns status %d", errDeadLink, context, status)
	}
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return "", false, fmt.Errorf("%w: %s returns status %d",
			errUnverifiable, context, status)
	}

	if !isRedirectStatus(status) {
		return "", false, nil
	}

	// resp.Location resolves a relative Location against the request URL;
	// returning the raw header stored values like "/new-path" as the bookmark.
	target, err := resp.Location()
	if err != nil {
		return "", false, nil //nolint:nilerr // a redirect with no usable Location is final
	}

	return target.String(), isPermanentRedirect(status), nil
}

// isDeadLinkStatus reports whether a status asserts the resource itself is
// gone, which is the only ground for deleting a bookmark.
//
// The set is deliberately just these two. Treating the whole 4xx range as dead
// deleted bookmarks over answers that describe the request rather than the
// resource: 429 and 408 are throttling — which this tool provokes itself, by
// checking many bookmarks on one host at once — 401 and 403 are access control,
// and 451 is a legal block, all of which leave the page in place. Every one of
// them is the "could not check" case that must be skipped and counted.
func isDeadLinkStatus(status int) bool {
	return status == http.StatusNotFound || status == http.StatusGone
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

// isPermanentRedirect reports whether a redirect moves the resource for good.
// RFC 9110 §15.4 gives that meaning to 301 and 308 only: after a 302 or 307 "the
// client ought to continue to use the target URI", and a 303 points at a
// different resource. A login wall answers an anonymous request with exactly
// those, so writing them back replaced the bookmark with the login page.
func isPermanentRedirect(status int) bool {
	return status == http.StatusMovedPermanently || status == http.StatusPermanentRedirect
}

// confirmWithGet re-requests a URL that refused HEAD, returning the GET status
// when one is obtained and the original status otherwise.
func (c *checker) confirmWithGet(urlString string, headStatus int) int {
	c.hosts.wait(urlString)

	resp, err := c.client.Get(urlString)
	if err != nil {
		return headStatus
	}
	defer c.closeBody(resp.Body)

	return resp.StatusCode
}

// fixParenthesesSuffix drops a trailing ")" when the URL works without it,
// repairing links copied out of Markdown.
//
// Only an unbalanced ")" is a candidate, the rule autolinkers use. A balanced
// one belongs to the URL — /wiki/Mercury_(planet), ?q=f(x) — and trimming it
// whenever the shorter URL also answered 200 rewrote live bookmarks to broken
// ones on any host with a catch-all route.
//
// It cannot fail: an unreachable trimmed URL leaves the original in place, and
// the liveness check that follows judges that original.
func (c *checker) fixParenthesesSuffix(urlString string) string {
	if !strings.HasSuffix(urlString, ")") ||
		strings.Count(urlString, "(") >= strings.Count(urlString, ")") {
		return urlString
	}

	updatedURL := strings.TrimSuffix(urlString, ")")
	if c.verbose {
		fmt.Fprintf(c.out, "URL '%s' ends with ')', retrying without it.\n", urlString)
	}

	c.hosts.wait(updatedURL)

	resp, err := c.client.Head(updatedURL)
	if err != nil {
		// The trimmed variant is unreachable; keep the original rather than
		// reporting the bookmark as broken.
		return urlString
	}
	defer c.closeBody(resp.Body)

	if resp.StatusCode == http.StatusOK {
		c.stats.IncrementParenthesesFixed()

		if c.verbose {
			fmt.Fprintf(c.out, "URL updated: '%s' -> '%s'\n", urlString, updatedURL)
		}

		return updatedURL
	}

	return urlString
}
