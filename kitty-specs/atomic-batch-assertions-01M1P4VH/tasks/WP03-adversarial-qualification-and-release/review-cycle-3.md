---
affected_files: []
cycle_number: 3
mission_slug: atomic-batch-assertions-01M1P4VH
reproduction_command:
reviewed_at: '2026-09-04T14:37:44Z'
reviewer_agent: user
wp_id: WP03
---

# WP03 Secondary Audit Feedback — Performance Campaign Reproducibility

Review target: candidate `8de0f7f549791d0299e84a15bbe6b256c8c7afdd`, evidence `4fefead8bb66d1cab58f2a4bbbb57e90cfa7ef1e`

The formal cycle-2 review passed an isolated 11-run command, but a concurrent independent audit reproduced an intermittent failure using the identical command: one window reported durable asserted p95 `4.815271ms` against control `4.803121ms`, exceeding the ratified strict `<4ms` bound. An immediate identical rerun passed. This is intermittent qualification, not a demonstrated production regression, but the sealed evidence claims `stability_runs=11` and `stability_passed=11` while committing raw data for only one three-window run. The validator cannot recompute the claimed 11-run census.

## Required repair

1. Execute the fixed 11-run stability campaign inside one test/program invocation with no retries or discarded runs.
2. Emit one machine-readable raw artifact containing all `11 × 3` windows, exact sample arrays/counts, warmup/configuration, and per-run verdict.
3. Recompute every nearest-rank p95 and both thresholds from every raw window in the evidence validator.
4. Require exactly 11 attempted and 11 passed runs; any failed window fails the campaign and cannot be omitted.
5. Lengthen the durable sampling window enough to make the reference-host gate reproducible under ordinary local scheduling noise. Do not loosen, average away, or conditionally skip the `<4ms` requirement.
6. Record relevant reference-host/load conditions and rerun the complete candidate publication, external consumption, gate, and evidence-sealing sequence at the new exact SHA.
7. Add semantic mutants for a missing run/window, altered run census, one failed durable window, and raw-sample/p95 mismatch.

The other six cycle-1 findings remain closed and must not regress.
