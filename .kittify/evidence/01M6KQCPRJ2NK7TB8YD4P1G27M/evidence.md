# Independent upstream GapDB duplicate exact-read wire repair review

PASS — approve the exact source fix technically. No remaining actionable finding in this three-file change. This is independent source review, not a new executable build, provider admission, application qualification, release or merge approval.

## Source and governed identity

Reviewed fixed branch `fix/readmany-duplicate-wire-error`, commit `e023161df9535817b380cc40e0c49bae490d8afc`, tree `7d4eced9d21fe5ba38e2fe8871f0fa46bd2009c8`, versus exact base `006e9d469d40fc5a6ce306a083ec96a98cc02e36`.

Own governed review Op `01M6KQCPRJ2NK7TB8YD4P1G27M`, actual profile reviewer-renata, opened with pinned private Spec Kitty5150CLI in the GapDB clone. Read full returned governance capsule, AGENTS.md, authoritative structured charter/project GAP001–007 directives, companion charter, required MVP spec, relevant protocol/error contract and exact change intent/diff. No author contribution or self-review occurred. Root's implementation Op remains separate and untouched.

Interpretation: duplicate exact-read keys are malformed get_many arguments at the protocol boundary; this is distinct from mutation/assertion duplicate-key diagnostics and from the unchanged first-party client's local validation. The change preserves existing preparse malformed-request envelope behavior and valid-request correlation, not a new correlation contract for undecodable requests.

Exact changed blobs:

- `internal/protocol/envelope.go`: `fbc4b852d9a81d3e788ea08245f7a02d94476214`, SHA256 `092adab6fa023b7fc9a8a118d994a3342359599f2a4429319e9d2bdc528b6077`.
- `internal/server/server_test.go`: `bd85b78d1ee81e866e1ab4f36c3a3f86678a9bbf`, SHA256 `12c398a555124c2dbb90a214d057775ec5cdfb718909373f46ddc8379b5d4840`.
- `tests/compatibility/protocol/read_many_error_test.go`: `8d3539dff4733493c78f2f48463acfb1234f3cd9`, SHA256 `118382c0677a7c5ca7177e39a176e67d704512b83de26c3f91841279c1cfbc69`.

## Intent and correctness

The sole production-line change replaces an index-free DUPLICATE_KEY with existing invalidProtocol(arguments.keys, must contain unique keys). DUPLICATE_KEY's existing wire evidence schema requires mutation_indexes or assertion_index; the exact-read decoder supplied neither. Strict EncodeResponse round-trips through DecodeResponse and rejected its own error, so server's preparse failure handler wrote no complete frame and continued waiting on the same connection.

INVALID_REQUEST already requires field/path plus reason and stable fix-request/abort actions. The existing constructor supplies all of them and RetryNever. This repairs the invalid decoder-to-encoder boundary without relaxing structured error validation, adding synthetic mutation indices, weakening malformed JSON validation, changing response schema or granting KV authority.

The new tests reconstruct the intended observable contract: adjacent/nonadjacent duplicates receive promptly framed structured refusal; decode-stage errors retain existing Status/emptyRequestID envelope; the same connection subsequently accepts a valid request and echoes its exact requestID/operation; ordered missing/present values and original KV revisions survive unchanged. Valid request code/correlation paths are untouched. Existing client-side local duplicate validation remains unchanged; do not claim every local DUPLICATE_KEY was removed.

No source/storage format, error-code registry, limits, module/toolchain requirements, golden fixtures or old wire evidence schema changed. Diff is exactly one production line plus the two relevant test additions. Generic MVP/versioned-error/zero-revision-on-invalid-request requirements support this repair; the existing unique-key protocol narrative remains accurate, so no new schema/documentation exception is needed.

## Independent actual verification

Exported exact fixed Git archive to private non-worktree `/tmp/gri-9uetrd7v/source`; archive SHA256 `6faa487112262e25ba810254d7b89e92ac5011ade794052a9360e45b91f2a3fa`. All exported committed source files rehashed unchanged after tests. Only an additional private reviewer test was created; no clone source edit/staging occurred.

Actual Go1.26.7 executable SHA256 `182d1dc98119d61a6241590e1aaca180d771dbcb1133b9846684aee82d457362`. Literal sanitized private HOME/buildcache/GOPATH, existing module cache as input, GOTOOLCHAINlocal/GOPROXYoff/GOSUMDBoff/-mod=readonly; no model/SDK/provider executable/ambient credentials. Ordinary go test compilation creates temporary test binaries, not a rebuilt distributable sidecar.

1. `go test -race -count=1 ./internal/server ./tests/compatibility/protocol -run '^Test(IRWire|ReadManyDuplicate)' -json`: exit0,7cases pass,0fail/skip/race.
2. Independent `TestIRWire` adds three duplicate layouts, including missing-key duplicates, repeated invalid→valid exchanges on one real Unix connection under500ms deadline, actual decoder→encoder refusal, raw valid response ordering/value/revision, final global/durable/record-count invariance, and continued rejection of index-free DUPLICATE_KEY by the strict response codec.
3. Exact old production envelope restored ONLY through a private Go overlay (byte-identical to base, one production branch difference); `go test -overlay ... -count=1 ./internal/server -run '^Test(IRWire|ReadManyDuplicateWireRefusal)' -json`: exit1,4case/subtest failures,0pass/skip. Independent codec assertion fails on wrong error; both actual raw server duplicate layouts fail to receive a complete refusal at3.01s. This is behavioral red, not compile/setup failure.
4. Focused `go vet ./internal/protocol ./internal/server ./tests/compatibility/protocol`: exit0, empty log.

Raw argv/environment/exits/logs/source census/probe and overlay hashes are retained in `/tmp/gri-9uetrd7v/`: `binding.json`, `focused-receipt.json`, `focused.jsonl`, `revert-receipt.json`, `revert.jsonl`, `vet-receipt.json`, `vet.log`, `final-facts.json`. Independent probe SHA256 `1f048bc2fac9490e640580a19d516724d7418941d11a90f371fa9a6a0c3bd2bc`.

## Author evidence independently audited

Rehashed all manifest-bound per-run receipts/logs/XML and recomputed actual testcase outcomes:

- Meaningful red2:6failing cases, including both real socket no-frame controls before production fix. Initial red's overlong-socket setup failures are retained but excluded from the behavioral wire proof.
- Focused green6pass; full2251cases=2249pass+2existing opt-in performance skips; full race same2251/2skips/0fail/errors; vet/staticcheck exit0. Existing performance skips are TestAssertionPerformancePairedSameHostWindows and TestReferencePerformanceProfile, not newly introduced or weakened gates.
- Receipts' Git HEAD006e9 denotes precommit worktree identity; final manifest binds exact committed three blobs, and production hashes/rawdiff track the actual change. No claim that tracked diff alone included the initially untracked test.

Computed evidence facts `/tmp/gri-9uetrd7v/author-evidence-verified.json`. I did not redundantly rerun both full2251suites after verifying them and independently reproducing focused race/wire/revert oracles. Reference performance campaigns were not enabled or newly qualified. Coverage percentage is not used as authority; the changed decoder decision is exercised directly and through real transport.

## Limits and follow-through

This source approval precedes ANY separately rebuilt provider. G3 current006e9client/currentHubnot and historical54d/947/M1/M2 admission records remain unchanged. New server executable identity/build evidence, reviewed plan/admission records, WP15actual qualification and final consumer/capacity gates remain separate. No main merge, push, GitHub approval, pin change, build/install or admission action was taken. Future PR remains draft/no rush so Stijn or his agents can pick it up when ready. Only my own Op is closed with this truthful report/source commit; author metadata remains unstaged for root.
