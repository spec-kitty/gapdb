# Spec Kitty Workaround Log

This log records Spec Kitty 3.2.6 defects, misleading output, and manual
recoveries encountered while delivering the Gapdb MVP mission. It is an
operational record, not part of the Gapdb product contract.

## 2026-08-23 — Repository bootstrap resolved outside the repository

- **Surface:** governed invocation dispatch during initial project bootstrap.
- **Symptom:** dispatch resolved `/home/lynn` and returned
  `NotInsideRepositoryError` while the intended repository was
  `/home/lynn/projects/gap`.
- **Likely trigger:** the repository had no initial commit and therefore no
  established branch ancestry for the resolver.
- **Recovery:** initialize Spec Kitty from the explicit repository root, create
  the charter and mission artifacts there, and retry after the repository had
  an initial commit.
- **Impact:** no product artifacts were lost; invocation setup required a
  manual retry.

## 2026-08-23 — Automatic commits could not bootstrap an unborn branch

- **Surface:** governed invocation close and charter safe-commit.
- **Symptom:** automatic commit creation failed while `main` was unborn.
- **Recovery:** create one narrowly scoped root commit containing the generated
  project charter, then resume Spec Kitty operations.
- **Impact:** git bootstrap was manual; generated artifact contents were not
  changed.

## 2026-08-23 — Protected-branch policy conflicted with mission bootstrap

- **Surface:** `spec-kitty safe-commit` for initial mission artifacts.
- **Symptom:** safe-commit correctly refused mission artifacts on protected
  `main`, although the new mission still needed a committed planning surface.
- **Recovery:** create `gapdb-mvp` and commit only the mission bootstrap
  artifacts there.
- **Impact:** introduced a temporary planning branch that later exposed a
  resolver inconsistency described below.

## 2026-08-23 — Plan setup reported contradictory commit and branch state

- **Surface:** plan setup and automatic plan commit.
- **Symptom:** setup reported current, target, planning, and merge branches as
  `gapdb-mvp`, but the automatic commit returned
  `no_op_wrong_surface` and described protected `main`. Later mission status
  still reported `main` as the target.
- **Recovery:** commit the plan artifacts with a targeted safe-commit to
  `gapdb-mvp` and preserve `main` as the mission's final merge target.
- **Impact:** plan artifacts were committed successfully; branch metadata was
  not consistently represented across commands.

## 2026-08-23 — Charter generation overstated selected governance

- **Surface:** first charter generation output.
- **Symptom:** output claimed all catalog paradigms and directives were active,
  while the authoritative governance selections were empty.
- **Recovery:** populate the intended narrow selections explicitly, regenerate,
  and verify the authoritative YAML and loaded charter context.
- **Impact:** no broad doctrine set remained active; the final charter contains
  only the reviewed selections.

## 2026-08-23 — Task preflight contradicted the completed plan phase

- **Surface:** `agent context resolve --action tasks` and
  `agent mission check-prerequisites`.
- **Symptom:** the resolver declared `main` to be the planning base and required
  checkout, with `branch_matches_target: false`, even though plan setup had
  generated and committed the complete mission planning surface on
  `gapdb-mvp`.
- **Recovery:** fast-forward `main` to the already reviewed `gapdb-mvp` planning
  commit before task generation. This makes the resolver's declared planning
  base contain the exact planning artifacts it requires while retaining the
  feature branch as a recovery reference.
- **Impact:** planning history remains linear and unchanged; task generation can
  proceed on the resolver's canonical branch.

## 2026-08-23 — Charter DRG status was misleading

- **Surface:** charter status.
- **Symptom:** status reported a missing synthesized DRG even though charter
  context loaded successfully and the project had no synthesized doctrine
  artifact requirement.
- **Recovery:** verify loaded charter context and authoritative charter
  freshness directly; treat the absent optional DRG as non-blocking.
- **Impact:** none on mission governance.

## 2026-08-23 — Requirement mapping refused the canonical protected branch

- **Surface:** `spec-kitty agent tasks map-requirements`.
- **Symptom:** after task preflight required `main` and reported
  `branch_matches_target: true`, requirement mapping refused to run because its
  default auto-commit policy forbids status mutations on protected `main`.
- **Recovery:** rerun the same mapping with the command's documented
  `--no-auto-commit` option, verify complete FR coverage, and leave finalization
  or a narrowly scoped manual commit to record the artifacts.
- **Impact:** no partial mutation occurred on the refused attempt; all 24
  functional requirements were mapped successfully on retry.

## 2026-08-23 — Task finalization partially mutated before protected-branch refusal

- **Surface:** `spec-kitty agent mission finalize-tasks`.
- **Symptom:** validate-only passed, but the mutating finalizer rewrote WP
  frontmatter and appended canonical task/WP events before its bookkeeping layer
  refused protected `main`. The documented `--target-branch gapdb-mvp` override
  did not affect the status-placement target and failed identically.
- **Recovery:** preserve the valid idempotent local mutations, return to the
  canonical `main` checkout, and rerun only the finalization command with Spec
  Kitty's supported one-command operator hatch,
  `SPEC_KITTY_ALLOW_PROTECTED_BRANCH_COMMITS=1`. Finalization then created
  `lanes.json`, the acceptance matrix, canonical state, and commit `97b0de6`.
- **Impact:** final task state is valid and fully committed. Branch protection was
  bypassed only for the required finalization command; project configuration was
  not weakened.

## 2026-08-23 — Runtime required a DRG that charter status treated as optional

- **Surface:** `spec-kitty next` charter preflight.
- **Symptom:** charter context and prior planning commands loaded successfully,
  but runtime advancement failed with `CHARTER_PREFLIGHT_FAILED` because the
  synthesized DRG was missing.
- **Recovery:** run the documented fresh-project
  `spec-kitty charter synthesize` path. It materialized the minimal doctrine
  provenance and synthesis manifest while retaining built-in doctrine fallback.
- **Impact:** no governance rule changed; runtime preflight can now resolve its
  required project doctrine root.

## 2026-08-23 — New runtime ignored completed lifecycle events

- **Surface:** first `spec-kitty next --result success` after synthesis.
- **Symptom:** despite committed `SpecifyCompleted`, `PlanCompleted`, and
  `TasksCompleted` canonical events, the new runtime run began at `discovery`.
- **Recovery:** reconcile each already-completed phase against its committed
  artifact instead of regenerating it. The newly enforced research CSV stubs
  were created and populated with the official sources already used by the
  plan, then the runtime phase was reported successful.
- **Impact:** reviewed artifacts were preserved; runtime bookkeeping is being
  advanced to the actual mission state.

## 2026-08-23 — Built-in workflow referenced a non-activated built-in profile

- **Surface:** software-dev runtime composition for `specify`.
- **Symptom:** composition requested built-in profile `researcher-robbie`, which
  exists in the installed doctrine catalog and projection manifest but was not
  activated by the generated charter. Runtime failed with
  `ProfileNotFoundError`.
- **Recovery:** activate `researcher-robbie` through
  `spec-kitty charter activate agent-profile researcher-robbie --resynthesize`.
  The charter and minimal DRG were regenerated through supported commands.
- **Impact:** no product scope changed; the workflow can resolve its own declared
  research profile.

## 2026-08-23 — Lane worktrees were not ignored by the initialized repository

- **Surface:** first implementation lane creation.
- **Symptom:** Spec Kitty created `.worktrees/<mission>-lane-a` inside the
  repository, but the initialized project had no `.gitignore` entry for
  `.worktrees/`, so the entire lane checkout appeared as untracked main-worktree
  content.
- **Recovery:** add the narrow repository-root ignore rule `.worktrees/` before
  any later targeted commit.
- **Impact:** execution worktrees remain visible to git's worktree machinery but
  cannot be accidentally swept into a project commit.

## 2026-08-23 — Implement prompt referenced an absent charter section

- **Surface:** WP01 implement governance instructions.
- **Symptom:** the runtime prompt required
  `spec-kitty charter context --include section:terminology-canon` when terms are
  introduced, but the command returned
  `No charter section found for selector 'section:terminology-canon'`.
- **Recovery:** use the committed mission specification, protocol/storage/error
  contracts, and data model as the terminology authority; require the implementer
  and reviewer to check for vocabulary drift explicitly.
- **Impact:** no terminology rule was bypassed and no new term was inferred from
  the missing section.

## 2026-08-23 — Pre-review gate referenced an absent charter section

- **Surface:** WP01 implement pre-review instructions.
- **Symptom:** the runtime prompt required
  `spec-kitty charter context --include section:code-review-checklist`, but the
  command returned
  `No charter section found for selector 'section:code-review-checklist'`.
- **Recovery:** apply the prompt's explicit definition of done, reviewer
  guidance, and charter directives directly; retain the independent review
  boundary and require the reviewer to inspect the entire WP01 diff and execute
  the prescribed verification suite.
- **Impact:** the missing generated selector did not relax the package gate or
  substitute implementer self-approval for independent review.

## 2026-08-23 — Pre-review coverage gate assumed Python infrastructure

- **Surface:** WP01 pre-review regression gate in a Go-only repository.
- **Symptom:** the generated gate returned `no_coverage` after importing the
  absent Python module `tests.architectural._gate_coverage`.
- **Recovery:** retain the gate result as runtime evidence and execute the
  repository-native full test suite, race detector, `go vet`, `staticcheck`,
  `govulncheck`, module verification, formatting, and diff checks.
- **Impact:** no product test was skipped; the generic architectural coverage
  hook could not assess this repository's Go tests.

## 2026-08-23 — Transition annotation was written after auto-commit

- **Surface:** WP01 `move-task --to for_review` bookkeeping.
- **Symptom:** the transition created commit `7200839`, then appended its note
  to `status.json` and `status.events.jsonl` after that commit, leaving both
  canonical status artifacts dirty on `main`.
- **Recovery:** preserve the append-only annotation and commit only those two
  status artifacts through the protected-branch one-command safe-commit path.
- **Impact:** WP01's lane and implementation commit remain clean; one extra
  targeted bookkeeping commit is required on the status authority branch.

## 2026-08-23 — Review prompt resolved the implementer identity

- **Surface:** WP01 independent review prompt composition.
- **Symptom:** the review claim and event selected `reviewer-renata`, but the
  generated prompt's identity block resolved `implementer-ivan` from the WP
  frontmatter.
- **Recovery:** bind the review to a separate agent instance, explicitly load
  the reviewer profile and full review contract, and record the actual reviewer
  identity in the canonical feedback body.
- **Impact:** the reviewer remained independent and rejected the package on five
  substantiated findings; only generated identity metadata was wrong.

## 2026-08-23 — First rejection was materialized as review cycle 2

- **Surface:** WP01 rejected-review artifact generation.
- **Symptom:** the prescribed `review-cycle-1.md` destination was copied by the
  runtime into `review-cycle-2.md`, whose frontmatter says cycle 2 while its
  body correctly identifies the first review cycle.
- **Recovery:** treat the committed `review-cycle-2.md` path reported by the
  runtime as canonical feedback and pass it unchanged into fix mode.
- **Impact:** feedback content and rejection state are intact; cycle numbering
  is inconsistent by one.

## 2026-08-23 — Review prompt referenced a third absent charter section

- **Surface:** WP01 terminology-cutover review instruction.
- **Symptom:** `section:regression-vigilance`, like the earlier terminology and
  checklist selectors, returned `No charter section found`.
- **Recovery:** no terminology cutover occurred; the reviewer nevertheless
  checked the diff against the committed spec, contracts, data model, and
  charter vocabulary.
- **Impact:** no vocabulary change escaped review.

## 2026-08-23 — Fix-mode prompt required a lane change that transition forbids

- **Surface:** WP01 fix-mode handoff and `move-task --to for_review`.
- **Symptom:** the implement prompt required reviewer profile/role frontmatter
  to be committed on the lane, while the transition validator rejected any
  committed `kitty-specs/` change on that lane.
- **Recovery:** commit the product repair first, then add a narrowly scoped
  follow-up lane commit removing only the transient two-line reviewer metadata
  diff. Preserve reviewer assignment through the review claim and independent
  agent context instead of WP frontmatter.
- **Impact:** the lane contains no net mission-metadata change and passed the
  transition validator; the known generated review-identity defect remains
  visible rather than being hidden by an invalid lane exception.

## 2026-08-23 — Historical rejection artifact blocked repaired approval

- **Surface:** WP01 repair-cycle approval transition.
- **Symptom:** after independent re-review closed every finding, the transition
  validator continued to treat the immutable historical
  `review-cycle-2.md` rejection as the current verdict and refused approval.
- **Recovery:** the reviewer used the command's explicit
  `--skip-review-artifact-check` option only after recording complete fresh
  approval evidence, including adversarial and deletion probes for all five
  findings. The canonical lane transition is `approved` in commit `676b29c`.
- **Impact:** status correctly records approval, but `tasks status` continues to
  display the historical rejection as a stale verdict.

## 2026-08-23 — Sync-state safe-commit dirties its own input

- **Surface:** targeted safe-commit of `.kittify/sync-state.json`.
- **Symptom:** committing the sync queue itself records the new local commit by
  rewriting the same file after the commit, so the worktree becomes dirty
  again. Repeating the operation is recursive and cannot produce a fixed point.
- **Recovery:** commit sync-state only when another canonical bookkeeping
  artifact must be reconciled; otherwise tolerate its generated post-commit
  queue update until a later governed commit or final handoff.
- **Impact:** no mission or product state is lost; a generated local-sync queue
  entry remains uncommitted between governed operations.

## 2026-08-23 — WP02 transition treated governance-only main history as stale code

- **Surface:** WP02 `move-task --to for_review` ancestry preflight.
- **Symptom:** lane B correctly began at approved WP01 product HEAD `8485a51`,
  but the preflight refused it as 26 commits behind `main`. Those commits were
  mission status, runtime, analysis, and workaround bookkeeping that must not be
  rebased into a product lane.
- **Recovery:** verify the lane's merge base and full WP01 product ancestry, keep
  the lane clean, and retry the exact transition with the command's `--force`
  option plus the established protected-main hatch. The forced event is
  `01M0QJBSKPV56S2W3BBC2ADR3G`.
- **Impact:** no product commit was skipped or duplicated; lane isolation was
  preserved while the canonical status authority advanced WP02 to review.
