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
- **URL Shortener Support**: Expands bit.ly, tinyurl.com, and is.gd URLs
- **Dead Link Detection**: Deletes bookmarks only on a definite 4xx (keeps 5xx for retry).
  Timeouts, DNS failures, and non-HTTP schemes are skipped, never deleted
- **URL Normalization**: Fixes common URL issues like trailing parentheses
- **Auto-Title Fetching**: Automatically fetches and sets page titles for bookmarks without them
- **Auto-Tagging**: Adds up to 10 suggested tags (from Pinboard API) for untagged bookmarks,
  including an `.autoTagged` marker. Pinboard treats tags beginning with a period as
  private tags
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
[releases page](https://github.com/ivuorinen/pinboard-checker/releases).

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

### Error Handling

This tool treats errors differently based on their type:

- **4xx errors (404, 410, etc.)**: Permanent client errors → Bookmark deleted
- **5xx errors (500, 503, etc.)**: Temporary server errors → Bookmark kept
- **Unverifiable**: timeouts, DNS failures, and URL schemes `net/http` cannot dial
  (`mailto:`, `ftp:`, `file:`, `javascript:` bookmarklets) → Bookmark kept and counted
  as skipped

This prevents accidental deletion during temporary server outages, and ensures the
tool never deletes a bookmark it was simply unable to check. A host that refuses
`HEAD` with 403 or 405 is re-checked with `GET` before any deletion.

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

## How It Works

1. **Fetch Bookmarks**: Retrieves all bookmarks from your Pinboard account
2. **Process Each Bookmark**:
   - Expands shortened URLs (bit.ly, tinyurl.com, is.gd)
   - Checks if the URL is accessible
   - Fixes trailing parentheses if needed
   - Follows the redirect chain to its final destination (up to 5 hops)
   - Validates final URL returns 2xx/3xx status
   - Fetches page title if bookmark has no title (unless -skip-titles)
   - Fetches suggested tags if bookmark has no tags (unless -skip-auto-tags)
     - Gets up to 9 tags from Pinboard's suggestion API, plus the `.autoTagged`
       marker, for 10 in total
     - If Pinboard suggests nothing, the bookmark is left untouched
3. **Update or Delete**:
   - If the URL changed: adds the bookmark at the new URL, then removes the old entry.
     Creation date, privacy, and read-later state are carried across
   - If only a title or tags were added: updates the bookmark in place
   - If URL is dead (4xx errors): Deletes bookmark (unless dry-run)
   - If server error (5xx) or unverifiable: Keeps bookmark

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
```

Both are enforced in CI. The suite uses local `httptest` servers only and
needs no network access.

### Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

MIT — see [LICENSE](LICENSE).

## API Rate Limiting

This tool respects Pinboard's API rate limit of 1 request per 3 seconds. The rate
limiter is built-in and ensures compliance even when processing bookmarks in parallel.
On an HTTP 429 the interval is doubled and the request retried, as Pinboard's API
documentation requires. Note that `posts/all` has its own limit of one call every
five minutes, so back-to-back runs may be throttled.

## Troubleshooting

### "Error: API token is required"

Set the `PINBOARD_API_TOKEN` environment variable or pass `-token` flag.

### Rate limit errors

The tool automatically rate-limits requests. If you see rate limit errors, there may
be other tools accessing your Pinboard account simultaneously.

### Timeouts

Requests time out after 15 seconds; change this with `-timeout` (for example
`-timeout 30s`). A timed-out URL is **skipped**, never deleted — only a definite
4xx response marks a bookmark as dead. Skipped bookmarks are reported in the
statistics block.
