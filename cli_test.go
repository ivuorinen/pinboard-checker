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
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// setArgs points os.Args at a fixed command line for one test. os.Args is an
// ordinary package variable, so main needs no seam of its own for this.
func setArgs(t *testing.T, args ...string) {
	t.Helper()
	swap(t, &os.Args, append([]string{"pinboard-checker"}, args...))
}

// isolatePacing restores the settings cli writes into package state.
//
// cli assigns perHostInterval and both client timeouts from its flags, so
// without this every cli test leaves the suite paced at the production
// defaults — which does not fail anything, it just silently makes every later
// test that touches a host sleep half a second.
func isolatePacing(t *testing.T) {
	t.Helper()
	swap(t, &perHostInterval, perHostInterval)
	swap(t, &httpClient.Timeout, httpClient.Timeout)
	swap(t, &redirectClient.Timeout, redirectClient.Timeout)
}

// The version flag short-circuits before anything touches the network or needs
// a token, so it must exit 0 even with no configuration at all.
//
//nolint:paralleltest // mutates package-level flag targets
func TestCLI_VersionExitsZero(t *testing.T) {
	isolatePacing(t)
	t.Setenv(tokenName, "")

	var code int

	out := captureOutput(func() {
		code = cli([]string{"-version"}, io.Discard)
	})

	if code != 0 {
		t.Errorf("cli(-version) = %d, want 0", code)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("cli(-version) printed nothing; it must print the version")
	}
}

// -h is a request for usage, not a failure, and must not be reported as one.
//
//nolint:paralleltest // mutates package-level flag targets
func TestCLI_HelpExitsZero(t *testing.T) {
	isolatePacing(t)
	t.Setenv(tokenName, "")

	if code := cli([]string{"-h"}, io.Discard); code != 0 {
		t.Errorf("cli(-h) = %d, want 0", code)
	}
}

// A malformed command line exits 2, the convention for a usage error, so a
// caller can tell "you invoked me wrong" from "the run failed".
//
//nolint:paralleltest // mutates package-level flag targets
func TestCLI_UnknownFlagExitsTwo(t *testing.T) {
	isolatePacing(t)
	t.Setenv(tokenName, "")

	if code := cli([]string{"-no-such-flag"}, io.Discard); code != 2 {
		t.Errorf("cli(-no-such-flag) = %d, want 2", code)
	}
}

// A rejected flag combination exits 1 without reaching the network.
//
//nolint:paralleltest // mutates package-level flag targets
func TestCLI_InvalidFlagsExitsOne(t *testing.T) {
	isolatePacing(t)
	t.Setenv(tokenName, "token-from-env")

	if code := cli([]string{"-workers", "0"}, io.Discard); code != 1 {
		t.Errorf("cli(-workers 0) = %d, want 1", code)
	}
}

// With no -token the environment supplies it, and the parsed flags must reach
// the package-level settings the rest of the run reads.
//
//nolint:paralleltest // mutates package-level flag targets and the HTTP clients
func TestCLI_AppliesFlagsAndUsesEnvToken(t *testing.T) {
	resetStats(t)
	setCIMode(t, true)
	isolatePacing(t)

	// An empty bookmark list: the run completes without any bookmark work, so
	// the test asserts the wiring rather than the processing.
	server := staticServer(t, http.StatusOK, `[]`)
	useEndpoint(t, server.URL)
	t.Setenv(tokenName, "token-from-env")

	code := cli([]string{"-timeout", "7s", "-host-interval", "250ms", "-ci"}, io.Discard)
	if code != 0 {
		t.Errorf("cli() = %d, want 0 for a clean run", code)
	}
	if httpClient.Timeout != 7*time.Second {
		t.Errorf("httpClient.Timeout = %s, want 7s from -timeout", httpClient.Timeout)
	}
	if perHostInterval != 250*time.Millisecond {
		t.Errorf("perHostInterval = %s, want 250ms from -host-interval", perHostInterval)
	}
}

// A failed fetch must exit non-zero: returning 0 let a broken scheduled run
// report success, which is the defect run()'s exit contract exists to prevent.
//
//nolint:paralleltest // mutates package-level flag targets
func TestCLI_FetchFailureExitsNonZero(t *testing.T) {
	resetStats(t)
	setCIMode(t, true)
	isolatePacing(t)

	server := staticServer(t, http.StatusUnauthorized, "")
	useEndpoint(t, server.URL)
	t.Setenv(tokenName, "token-from-env")

	if code := cli([]string{"-ci"}, io.Discard); code == 0 {
		t.Error("cli() = 0 after a failed fetch, want non-zero")
	}
}

// main must hand cli's exit code to os.Exit unchanged. The osExit seam is the
// only reason this is checkable at all: the real one would end the test binary.
//
//nolint:paralleltest // mutates os.Args and the exit seam
func TestMainPassesExitCodeThrough(t *testing.T) {
	got := -1

	isolatePacing(t)
	swap(t, &osExit, func(code int) { got = code })
	setArgs(t, "-version")

	captureOutput(main)

	if got != 0 {
		t.Errorf("main() exited %d, want 0 for -version", got)
	}
}

//nolint:paralleltest // mutates os.Args and the exit seam
func TestMainPropagatesFailure(t *testing.T) {
	got := -1

	isolatePacing(t)
	swap(t, &osExit, func(code int) { got = code })
	setArgs(t, "-workers", "0")
	t.Setenv(tokenName, "token-from-env")

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
