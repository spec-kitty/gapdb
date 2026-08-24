# Mission Review Exception: Python-Only Dead-Code Scanner

- **Scope:** `spec-kitty review --mode post-merge` gate 2 only.
- **Diagnostic:** `MISSION_REVIEW_DEAD_CODE_UNDETERMINABLE`.
- **Reason:** Spec Kitty 3.2.6 recognizes changed Python source files for this
  gate. Gapdb is a Go module, so its correct merged source set contains no
  supported Python files and the scanner cannot render a verdict.
- **Not waived:** dead code, unused implementation, uncalled production paths,
  or any other Gapdb quality requirement.
- **Compensating verification:** every WP review applied the dead-code and live
  production-caller checklist; the Go compiler, `go vet ./...`,
  `staticcheck ./...`, `go test ./...`, and `go test -race ./...` are required
  to pass post-merge. Public API surfaces are exercised through the Unix server,
  first-party client, CLI, contract/adoption runners, and executable examples.
- **Owner:** Gapdb mission review.
- **Recorded:** 2026-08-23 (America/Chicago).

This exception is language-scoped and expires if Spec Kitty gains a Go-aware
dead-code gate. It does not authorize a passing verdict if the compensating Go
checks fail.
