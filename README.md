# Pinboard Checker

A Go tool to check and maintain your [Pinboard](https://pinboard.in) bookmarks by:
- Expanding shortened URLs (bit.ly, tinyurl.com, is.gd)
- Following redirects to final destinations
- Fixing URLs with trailing parentheses
- Detecting and removing dead links
- Fetching and setting missing bookmark titles
- Adding suggested tags to untagged bookmarks
- Updating bookmarks with corrected URLs

## Features

- **Parallel Processing**: Processes bookmarks concurrently (configurable, default 10 workers)
- **Rate Limiting**: Respects Pinboard API rate limits (1 request per 3 seconds)
- **URL Shortener Support**: Expands tinyurl.com and is.gd URLs, and bit.ly when
  `BITLY_ACCESS_TOKEN` is set. An unexpandable short link is left as-is, never deleted
- **Dead Link Detection**: Deletes bookmarks only on 404 or 410 — the two answers that
  report the resource itself gone. Every other status, timeouts, DNS failures, and
  non-HTTP schemes are skipped, never deleted
- **URL Normalization**: Fixes common URL issues like trailing parentheses
- **Auto-Title Fetching**: Automatically fetches and sets page titles for bookmarks without them
- **Auto-Tagging**: Adds up to 9 suggested tags (from Pinboard API) plus an
  `.autoTagged` marker to untagged bookmarks, 10 in total. Pinboard treats tags
  beginning with a period as private tags
- **Dry Run Mode**: Test changes without modifying bookmarks
- **Progress Tracking**: Shows progress bar for batch operations
- **CI Mode**: Quiet mode for automated environments

## Installation

### From Source

```bash
go install github.com/ivuorinen/pinboard-checker@latest
```

### From Release

Download the latest binary for your platform from the
[releases page](https://github.com/ivuorinen/pinboard-checker/releases). Every
release ships archives, `.deb`/`.rpm`/`.apk` packages, SBOMs, and a
`checksums.txt` signed keylessly with [cosign](https://github.com/sigstore/cosign):

```bash
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/ivuorinen/pinboard-checker/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

## Configuration

You need a Pinboard API token to use this tool. Get yours from:
https://pinboard.in/settings/password

### Environment Variable

Create a `.env` file:

```bash
PINBOARD_API_TOKEN=username:XXXXXXXXXXXXXXXXXXXX

# Optional: without it, bit.ly links are left unexpanded (never deleted),
# because Bitly's expand API rejects unauthenticated requests.
BITLY_ACCESS_TOKEN=
```

Or set it in your shell:

```bash
export PINBOARD_API_TOKEN=username:XXXXXXXXXXXXXXXXXXXX
```

### Command Line Flag

```bash
pinboard-checker -token=username:XXXXXXXXXXXXXXXXXXXX
```

## Security Best Practices

### API Token Security

**Important**: Your Pinboard API token provides full access to your bookmarks.

- **Never commit tokens**: Add `.env` to `.gitignore` (already configured)
- **Keep tokens secure**: Treat them like passwords
- **Use environment variables**: Prefer `.env` file or shell environment over command-line
  flags, which leak the token into your shell history and the process table
- **Rotate if compromised**: Generate a new token at https://pinboard.in/settings/password

### API Design Limitation

Pinboard's API requires authentication tokens to be passed as URL query parameters
(not headers). This means:

- Tokens may appear in HTTP server logs (your web server, proxies, etc.)
- This is a limitation of Pinboard's API design, not this tool
- **Always use HTTPS** (enforced by Pinboard API)
- The tool URL-encodes all parameters for safety
- Error messages strip the request URL, so a failed request cannot print your
  token to stderr or into a CI log

### Error Handling

This tool treats errors differently based on their type:

- **404 and 410**: the server reports the resource itself gone → Bookmark deleted
- **Every other 4xx (401, 403, 408, 429, 451, …)**: the server refused *the request*,
  not the resource → Bookmark kept and counted as skipped
- **5xx errors (500, 503, etc.)**: Temporary server errors → Bookmark kept
- **Unverifiable**: timeouts, DNS failures, and URL schemes `net/http` cannot dial
  (`mailto:`, `ftp:`, `file:`, `javascript:` bookmarklets) → Bookmark kept and counted
  as skipped

This prevents accidental deletion during temporary server outages, and ensures the
tool never deletes a bookmark it was simply unable to check. Rate limiting (429) is
the case that matters most in practice: a host answering 429 is throttling this
tool, and says nothing at all about whether the page is still there.

## Usage

### Basic Usage

```bash
# Check and update bookmarks (reads token from .env or environment)
pinboard-checker

# Use explicit token
pinboard-checker -token=username:XXXXXXXXXXXXXXXXXXXX

# Dry run (show what would change without modifying)
pinboard-checker -dry-run

# Verbose output
pinboard-checker -verbose

# CI mode (no progress bar, minimal output)
pinboard-checker -ci
```

### Options

- `-token string`: Pinboard API token (default: reads from `PINBOARD_API_TOKEN` env var)
- `-dry-run`: Run without making changes (default: false)
- `-verbose`: Enable verbose logging (default: false)
- `-ci`: CI mode - no progress bar or verbose output (default: false)
- `-skip-titles`: Skip fetching titles for bookmarks without them (default: false)
- `-skip-auto-tags`: Skip auto-tagging bookmarks without tags (default: false)
- `-workers int`: Number of concurrent workers (default: 10, allowed range: 1-100)
- `-timeout duration`: Per-request HTTP timeout (default: 15s)
- `-host-interval duration`: Minimum delay between requests to any one bookmarked
  host (default: 500ms; `0` disables the throttle)
- `-version`: Print the version and exit

## How It Works

1. **Fetch Bookmarks**: Retrieves all bookmarks from your Pinboard account
2. **Process Each Bookmark**:
   - Skips anything `net/http` cannot dial (`mailto:`, `ftp:`, `file:`,
     `javascript:` bookmarklets) without touching it
   - Expands shortened URLs (bit.ly, tinyurl.com, is.gd)
   - Fixes a trailing parenthesis when the URL works without it
   - Follows the redirect chain to its final destination (up to 5 hops)
   - Checks the **final** URL, and only it: a 404 or 410 marks the bookmark dead,
     anything else keeps it. The repairs above run first on purpose — their whole
     point is to turn a URL that fails right now into one that works
   - Fetches page title if bookmark has no title (unless `-skip-titles`)
   - Fetches suggested tags if bookmark has no tags (unless `-skip-auto-tags`)
     - Gets up to 9 tags from Pinboard's suggestion API, plus the `.autoTagged`
       marker, for 10 in total
     - If Pinboard suggests nothing, the bookmark is left untouched
3. **Update or Delete**:
   - If the URL changed: adds the bookmark at the new URL, then removes the old entry.
     Creation date, privacy, and read-later state are carried across
   - If only a title or tags were added: updates the bookmark in place
   - If URL is dead (404 or 410): Deletes bookmark (unless dry-run)
   - If any other status or unverifiable: Keeps bookmark

## Examples

### Check bookmarks verbosely

```bash
pinboard-checker -verbose
```

### Test without making changes

```bash
pinboard-checker -dry-run -verbose
```

### Run in CI/CD pipeline

```bash
pinboard-checker -ci
```

Exits non-zero when the bookmark fetch fails or when any change could not be
applied, so a broken scheduled run does not report success. Failure counts are
printed even under `-ci`.

### Skip fetching missing titles

```bash
pinboard-checker -skip-titles
```

### Skip auto-tagging bookmarks

```bash
pinboard-checker -skip-auto-tags
```

## Statistics

Every run except `-ci` prints a summary: timings, the action breakdown, what
changed, HTTP status counts per operation, and the API calls performed. Two
counters are worth knowing:

- **Skipped (unchecked)** — bookmarks left untouched because they could not be
  verified: an unreachable host, a timeout, or a scheme `net/http` cannot dial.
  These are never deletion candidates.
- **Writes rejected** — changes Pinboard refused. This is also what makes the
  process exit non-zero, and it is printed even under `-ci`.
- **Duplicates merged** — bookmarks whose corrected URL turned out to be one you
  had already bookmarked. The existing entry is left exactly as it is, notes and
  tags included, and only the duplicate is removed.

The report is deterministic: identical counters render identical text, so two
runs can be diffed against each other.

## Development

### Requirements

- Go — see the `go` directive in [`go.mod`](go.mod)

### Building

```bash
go build -o pinboard-checker
```

### Testing

```bash
go test -race ./...
golangci-lint run
just coverage        # tests + the 100% gate + the HTML report
```

All three are enforced in CI. The suite uses local `httptest` servers only and
needs no network access.

### Coverage

Statement coverage is 100% and CI fails below it, so a new branch arrives with
its test or not at all. Go has no coverage pragma, so a genuinely unreachable
block is excluded with a marker placed inside it:

```go
if err != nil {
    // coverage:ignore json.Marshal cannot fail for a map[string]string
    return "", fmt.Errorf("failed to encode bit.ly request: %w", err)
}
```

There is exactly one of these today. It is for code that *cannot* run, never for
code that is merely awkward to reach — treat a new one as a claim to verify.
`scripts/coverage-gate.sh` implements the rule and reports what it excluded.

### Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Make your change, keeping `go test -race ./...` and `golangci-lint run` green
4. Commit using [Conventional Commits](https://www.conventionalcommits.org/)
   (`fix:`, `feat:`, `docs:`, `chore:`, `ci:`, …), as the existing history does
5. Push to the branch (`git push origin feature/amazing-feature`)
6. Open a Pull Request

CI runs the build, the test suite under `-race`, and `golangci-lint`. All three
must pass.

## API Rate Limiting

This tool respects Pinboard's API rate limit of 1 request per 3 seconds. The rate
limiter is built-in and ensures compliance even when processing bookmarks in parallel.
On an HTTP 429 the interval is doubled and the request retried, as Pinboard's API
documentation requires — up to 5 attempts, and never beyond a 60-second ceiling.
Once requests succeed again the interval halves back toward the 3-second default,
so a momentary throttle does not slow the rest of the run. If all 5 attempts are
rejected the run reports a rate-limit error rather than continuing silently.

Note that `posts/all` has its own limit of one call every five minutes, so
back-to-back runs may be throttled on the very first request.

### Bookmarked hosts

Separately from the Pinboard budget, requests to any single bookmarked host are
spaced 500ms apart. Without this, an account with many bookmarks on one domain
had its workers send them all at once, and the domain answered 429 or 403 — an
answer the tool provoked itself. Different hosts are never throttled against
each other, so the pool still runs at full width. Every request also carries a
`User-Agent` naming the tool and this repository, so a host that does throttle
can see what is calling it.

Tune it with `-host-interval`:

```bash
# Gentler: one request per host every two seconds
pinboard-checker -host-interval 2s

# Off: no per-host pacing at all
pinboard-checker -host-interval 0
```

The trade-off is run time against how hard you lean on the hosts you have most
bookmarks on. At the 500ms default, 40 bookmarks on one domain take about 20
seconds to check; at `0` they go out as fast as the workers can send them, which
is what produced the 429s that used to be read as dead links. Raise it if a host
still throttles you, lower it only for accounts spread thin across many domains.
Bookmarks on different hosts are unaffected either way.

## Troubleshooting

### "Error: API token is required"

Set the `PINBOARD_API_TOKEN` environment variable or pass `-token` flag.

### Rate limit errors

The tool rate-limits itself and backs off automatically on HTTP 429, so occasional
throttling needs no intervention. Persistent rate limiting usually means another
client is using the same Pinboard account, or that the run started within five
minutes of a previous one — `posts/all` allows one call per five minutes.

### Timeouts

Requests time out after 15 seconds; change this with `-timeout` (for example
`-timeout 30s`). A timed-out URL is **skipped**, never deleted — only a 404 or 410
marks a bookmark as dead. Skipped bookmarks are reported in the statistics block.

## License

MIT — see [LICENSE](LICENSE).
