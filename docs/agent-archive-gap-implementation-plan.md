# Archive implementation gap plan

Baseline: main `62d4674`, audited September 21, 2026. Preserve local user edits; implement on isolated branches. This work closes the audit findings without changing the archive's source-first, single-owner design.

| PR | Scope | Dependencies | Acceptance |
| --- | --- | --- | --- |
| 1 | Durable pending publications, accumulated hook evidence, bounded capture, rewrite detection | main | Restart retries preserve exact bytes; rate limits cannot lose evidence; truncated history cannot replace richer evidence |
| 2 | Parser-only metadata regeneration | 1 | Source hash and capture time stay fixed; metadata-only retries are durable |
| 3 | Safe retention | main | Current and predecessor survive; older snapshots respect grace; expiry removes metadata before sources and retries safely |
| 4 | Evaluation evidence | 1 where integration requires it | Observed skill inventory and filtered snapshots retain provenance; feedback is explicit; supported hook fields survive filtering |
| 5 | Status evidence and legacy migration | 1, 2, 4 | Status separates capture/publication/verification with age and identity; legacy owned job is deliberately retired |
| 6 | Release gate and documentation | main; final ledger includes all PRs | Public release fails closed without signing/notarization; remaining live checks are honest |

For each slice: write regression tests, run focused checks, review the diff, and open a reviewable PR. Run race tests and vet against the final integrated stack, plus repository checks and release builds where available. Do not merge without user instruction.

Live verification is a separate acceptance boundary: start with synthetic R2 sessions; AWS verification is deferred by the user. Real app trust/hooks, background Keychain access, two physical Macs, measured hook latency, and signed/notarized downloads must not be claimed from unit tests. No private conversation upload or real hook installation is required to build these PRs.
