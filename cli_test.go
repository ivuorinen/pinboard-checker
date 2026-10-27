// cli_test.go covers the command-line entry point: flag parsing, the exit-code
// contract, and main itself.
//
// The exit code is this tool's whole interface to a scheduler or a CI job, and
// before these tests it had no coverage at all — a run that failed to fetch
// anything could have started exiting 0 without a single test noticing.
package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// Fixtures for the token-resolution tests.
const (
	tokenFlag   = "-token"
	dotEnvToken = "user:abc"
	envToken    = "from-env"
)

// setArgs points os.Args at a fixed command line for one test. os.Args is an
// ordinary package variable, so main needs no seam of its own for this.
func setArgs(t *testing.T, args ...string) {
	t.Helper()
	swap(t, &os.Args, append([]string{"pinboard-checker"}, args...))
}

// The version flag short-circuits before anything touches the network or needs
// a token, so it must exit 0 even with no configuration at all.
func TestCLI_VersionExitsZero(t *testing.T) {
	t.Parallel()

	out := &lockedBuffer{}

	if code := cli([]string{"-version"}, out, io.Discard); code != 0 {
		t.Errorf("cli(-version) = %d, want 0", code)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Error("cli(-version) printed nothing; it must print the version")
	}
}

// -h is a request for usage, not a failure, and must not be reported as one.
func TestCLI_HelpExitsZero(t *testing.T) {
	t.Parallel()

	if code := cli([]string{"-h"}, io.Discard, io.Discard); code != 0 {
		t.Errorf("cli(-h) = %d, want 0", code)
	}
}

// A malformed command line exits 2, the convention for a usage error, so a
// caller can tell "you invoked me wrong" from "the run failed".
func TestCLI_UnknownFlagExitsTwo(t *testing.T) {
	t.Parallel()

	if code := cli([]string{"-no-such-flag"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("cli(-no-such-flag) = %d, want 2", code)
	}
}

// A rejected flag combination exits 1 without reaching the network, and the
// reason goes to the stderr cli was handed: it used to go to the process's own
// stderr whatever the caller passed.
func TestCLI_InvalidFlagsExitsOne(t *testing.T) {
	t.Parallel()

	stderr := &lockedBuffer{}

	if code := cli([]string{tokenFlag, "x", "-workers", "0"}, io.Discard, stderr); code != 1 {
		t.Errorf("cli(-workers 0) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "-workers must be between") {
		t.Errorf("stderr = %q, want the -workers error", stderr.String())
	}
}

// With no -token the environment supplies it, and every parsed flag must reach
// the checker the run uses.
//
//nolint:paralleltest // uses t.Setenv
func TestConfigure_AppliesFlagsAndUsesEnvToken(t *testing.T) {
	t.Setenv(tokenName, "token-from-env")

	chk, code := configure([]string{
		"-timeout", "7s", "-host-interval", "250ms", "-workers", "3",
		"-ci", "-verbose", "-dry-run", "-skip-titles", "-skip-auto-tags",
	}, io.Discard, io.Discard)
	if chk == nil {
		t.Fatalf("configure() = nil, %d; want a checker", code)
	}

	if chk.apiToken != "token-from-env" {
		t.Errorf("apiToken = %q, want the environment's", chk.apiToken)
	}
	if chk.client.Timeout != 7*time.Second || chk.redirectClient.Timeout != 7*time.Second {
		t.Errorf("client timeouts = %s, %s; want 7s from -timeout",
			chk.client.Timeout, chk.redirectClient.Timeout)
	}
	if chk.hosts.interval != 250*time.Millisecond {
		t.Errorf("host interval = %s, want 250ms from -host-interval", chk.hosts.interval)
	}
	if chk.workers != 3 {
		t.Errorf("workers = %d, want 3", chk.workers)
	}
	if !chk.ciMode || !chk.verbose || !chk.dryRun || !chk.skipTitles || !chk.skipAutoTags {
		t.Errorf("mode flags = ci %v, verbose %v, dry-run %v, skip-titles %v, "+
			"skip-auto-tags %v; want all true",
			chk.ciMode, chk.verbose, chk.dryRun, chk.skipTitles, chk.skipAutoTags)
	}
}

// A failed fetch must exit non-zero: returning 0 let a broken scheduled run
// report success, which is the defect run()'s exit contract exists to prevent.
//
//nolint:paralleltest // repoints the process-level pinboardAPIEndpoint
func TestCLI_FetchFailureExitsNonZero(t *testing.T) {
	swap(t, &pinboardAPIEndpoint, staticServer(t, http.StatusUnauthorized, "").URL)

	stderr := &lockedBuffer{}

	if code := cli([]string{tokenFlag, "x", "-ci"}, io.Discard, stderr); code == 0 {
		t.Error("cli() = 0 after a failed fetch, want non-zero")
	}
	if !strings.Contains(stderr.String(), "Error fetching bookmarks") {
		t.Errorf("stderr = %q, want the fetch error", stderr.String())
	}
}

// writeDotEnv writes content to a .env file in a fresh directory and returns
// the file's path.
func writeDotEnv(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write .env: %v", err)
	}

	return path
}

// Every form the README promises reads the token; anything else is taken
// literally rather than guessed at.
func TestTokenFromDotEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "plain", content: "PINBOARD_API_TOKEN=user:abc", want: dotEnvToken},
		{name: "export prefix", content: "export PINBOARD_API_TOKEN=user:abc", want: dotEnvToken},
		{name: "double quotes", content: `PINBOARD_API_TOKEN="user:abc"`, want: dotEnvToken},
		{name: "single quotes", content: `PINBOARD_API_TOKEN='user:abc'`, want: dotEnvToken},
		{name: "quoted with a comment after", content: `PINBOARD_API_TOKEN="user:a#c" # mine`,
			want: "user:a#c"},
		{name: "unquoted with a comment after", content: "PINBOARD_API_TOKEN=user:abc # mine",
			want: dotEnvToken},
		{name: "spaces around the equals sign", content: "PINBOARD_API_TOKEN = user:abc",
			want: dotEnvToken},
		{name: "comments, blank lines, other keys and CRLF",
			content: "# token\r\n\r\nOTHER=1\r\nPINBOARD_API_TOKEN=user:abc\r\n", want: dotEnvToken},
		{name: "byte-order mark", content: byteOrderMark + "PINBOARD_API_TOKEN=user:abc",
			want: dotEnvToken},
		{name: "an equals sign inside the value", content: "PINBOARD_API_TOKEN=user:abc==",
			want: "user:abc=="},
		{name: "the last definition wins",
			content: "PINBOARD_API_TOKEN=user:old\nPINBOARD_API_TOKEN=user:new", want: "user:new"},
		{name: "empty quotes", content: `PINBOARD_API_TOKEN=""`, want: ""},
		{name: "an unterminated quote is kept literally", content: `PINBOARD_API_TOKEN="user:abc`,
			want: `"user:abc`},
		{name: "variable expansion is not performed", content: "PINBOARD_API_TOKEN=${USER}",
			want: "${USER}"},
		{name: "a file without the key", content: "OTHER=1", want: ""},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := tokenFromDotEnv(writeDotEnv(t, testCase.content))
			if err != nil {
				t.Fatalf("tokenFromDotEnv() error = %v, want nil", err)
			}
			if got != testCase.want {
				t.Errorf("tokenFromDotEnv() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// A line without "=" is a typo, not a comment: skipping it turned
// "PINBOARD_API_TOKEN user:abc" into an unset token. The error names the line.
func TestTokenFromDotEnv_MalformedLineIsAnError(t *testing.T) {
	t.Parallel()

	_, err := tokenFromDotEnv(writeDotEnv(t, "OTHER=1\nPINBOARD_API_TOKEN user:abc\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("tokenFromDotEnv() error = %v, want one naming line 2", err)
	}
}

// .env is optional: a missing file is no token, not an error.
func TestTokenFromDotEnv_MissingFileIsNotAnError(t *testing.T) {
	t.Parallel()

	got, err := tokenFromDotEnv(filepath.Join(t.TempDir(), ".env"))
	if err != nil || got != "" {
		t.Errorf("tokenFromDotEnv() = %q, %v; want \"\", nil", got, err)
	}
}

// A .env that exists but cannot be read is an error. A directory stands in for
// it: unlike a permission bit, it fails the same way when the suite runs as
// root.
func TestTokenFromDotEnv_UnreadableFileIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := tokenFromDotEnv(t.TempDir()); err == nil {
		t.Error("tokenFromDotEnv(directory) error = nil, want a read error")
	}
}

// Reading the token must not export the file's other keys: a general loader
// copied every one into the environment, HTTPS_PROXY and SSL_CERT_FILE
// included.
func TestTokenFromDotEnv_SetsNoEnvironment(t *testing.T) {
	t.Parallel()

	const probe = "PINBOARD_CHECKER_DOTENV_PROBE"

	if _, err := tokenFromDotEnv(writeDotEnv(t, probe+"=1\nPINBOARD_API_TOKEN=x")); err != nil {
		t.Fatalf("tokenFromDotEnv() error = %v, want nil", err)
	}
	if value, set := os.LookupEnv(probe); set {
		t.Errorf("%s = %q was exported; the file must only be read", probe, value)
	}
}

// -token wins, then the environment, then .env.
//
//nolint:paralleltest // uses t.Chdir and t.Setenv
func TestConfigure_TokenPrecedence(t *testing.T) {
	t.Chdir(filepath.Dir(writeDotEnv(t, "PINBOARD_API_TOKEN=from-file")))

	tests := []struct {
		name string
		env  string
		args []string
		want string
	}{
		{name: ".env when nothing else is set", want: "from-file"},
		{name: "the environment over .env", env: envToken, want: envToken},
		{name: "-token over both", env: envToken, args: []string{tokenFlag, "from-flag"},
			want: "from-flag"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(tokenName, testCase.env)

			chk, code := configure(testCase.args, io.Discard, io.Discard)
			if chk == nil {
				t.Fatalf("configure() = nil, %d; want a checker", code)
			}
			if chk.apiToken != testCase.want {
				t.Errorf("apiToken = %q, want %q", chk.apiToken, testCase.want)
			}
		})
	}
}

// A malformed .env fails the run with the line named, rather than leaving the
// token unset and blaming the user for not setting one.
//
//nolint:paralleltest // uses t.Chdir and t.Setenv
func TestCLI_MalformedDotEnvExitsOne(t *testing.T) {
	t.Chdir(filepath.Dir(writeDotEnv(t, "PINBOARD_API_TOKEN=user:abc\nthis is not valid\n")))
	t.Setenv(tokenName, "")

	stderr := &lockedBuffer{}

	if code := cli(nil, io.Discard, stderr); code != 1 {
		t.Errorf("cli() = %d, want 1 for a malformed .env", code)
	}
	if !strings.Contains(stderr.String(), ".env line 2") {
		t.Errorf("stderr = %q, want the .env error naming line 2", stderr.String())
	}
}

// main must hand cli's exit code to os.Exit unchanged. The osExit seam is the
// only reason this is checkable at all: the real one would end the test binary.
//
//nolint:paralleltest // mutates os.Args and the exit seam
func TestMainPassesExitCodeThrough(t *testing.T) {
	got := -1

	swap(t, &osExit, func(code int) { got = code })
	setArgs(t, "-version")

	main()

	if got != 0 {
		t.Errorf("main() exited %d, want 0 for -version", got)
	}
}

//nolint:paralleltest // mutates os.Args and the exit seam
func TestMainPropagatesFailure(t *testing.T) {
	got := -1

	swap(t, &osExit, func(code int) { got = code })
	setArgs(t, tokenFlag, "x", "-workers", "0")

	main()

	if got != 1 {
		t.Errorf("main() exited %d, want 1 for an invalid flag combination", got)
	}
}

// A binary with neither a stamped version nor build info reports "dev". A test
// binary always has build info, so the fallback needs the seam to be reachable.
//
//nolint:paralleltest // mutates the package-level version and build-info seam
func TestVersionString_FallsBackToDev(t *testing.T) {
	swap(t, &version, "")
	swap(t, &readBuildInfo, func() (*debug.BuildInfo, bool) { return nil, false })

	if got := versionString(); got != "dev" {
		t.Errorf("versionString() = %q, want %q", got, "dev")
	}

	// Build info present but with an empty version is the `go build` case, and
	// must fall through to the same answer rather than reporting "".
	swap(t, &readBuildInfo, func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{}, true
	})

	if got := versionString(); got != "dev" {
		t.Errorf("versionString() = %q, want %q for empty build info", got, "dev")
	}
}
