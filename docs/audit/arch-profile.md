# Architecture Profile

Generated: 2026-10-27

Detected: none
Confidence: none — manual review required

## Detected Patterns

No catalogued pattern has a signal. The module is one package (`main`) in two
source files (`main.go`, `statistics.go`) with no subdirectories of code. Imports are the
standard library plus `x/net/html`; there is no internal import graph to give a
dependency direction.

## Detected Combination

None.

## Inferred Structural Rules

None. No catalogued pattern reached Medium confidence, so no rule is inferred.

Observed, not inferred:

- One run is one `checker` value (main.go), built by `configure` from the command line;
  every collaborator and every piece of run state hangs off it, so no package-level
  mutable state is shared between runs or tests. The process-level seams that remain —
  `osExit`, `readBuildInfo`, `version`, `pinboardAPIEndpoint` — are read once per process.
- `checker.run` splits the work into a read-only phase (`processBookmarksParallel`, which
  may only read from Pinboard) and a sequential write phase (`applyBookmarkActions`).
  `pinboardWrite` is reachable only from the phase-two methods (`applyUpdateWrite`,
  `mergeIntoExisting`, `applyDeleteAction`). That plan/apply split is the program's
  load-bearing boundary; it is not a catalogued pattern and is recorded here for
  `/nitpicker arch` to check by hand.

## Ambiguities & Contradictions

None.

## Drift

Since 2026-10-26: the Low-confidence Pipe and Filter signal (`urlStep`, `repairSteps`) is
gone — the shortener expanders were deleted and `expandAndCheckURL` now calls
`fixParenthesesSuffix` and `followRedirects` directly. Run state moved from package
globals onto `checker`. The `godotenv` and `progressbar/v3` imports were replaced by
`tokenFromDotEnv` and `progressBar` in main.go.
