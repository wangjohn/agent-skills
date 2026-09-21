# Remediation acceptance record

Date: 2026-09-21. This record distinguishes implementation, automated tests, and live acceptance. It supersedes completion claims in older implementation notes; historical notes are not release approval.

## Pull requests

| Plan | Audit | PR | Base | Result |
| --- | --- | --- | --- | --- |
| P1 | A1 | [#28](https://github.com/wangjohn/agent-skills/pull/28) | #27 | Production-shaped relative probe keys and scheduled collection regression |
| P2 | A2 | [#30](https://github.com/wangjohn/agent-skills/pull/30) | #28 | Proven fresh starts; ambiguous first-seen Cursor capture rejected |
| P3 | A4 | [#31](https://github.com/wangjohn/agent-skills/pull/31) | #30 | Bounded version discovery and explicit capability evidence |
| P4 | A3 | [#33](https://github.com/wangjohn/agent-skills/pull/33) | #31 | Per app/project verification and scoped invalidation |
| P5 | A5, A6, A11 | [#32](https://github.com/wangjohn/agent-skills/pull/32) | #31 | Lifecycle/turn outcome, unknown text counts, pinned OTel conventions |
| P6 | A7, A8 | [#34](https://github.com/wangjohn/agent-skills/pull/34) | #32 | Exact hash filtering; honest partial eligibility evidence; A7 capability blocked |
| P7 | A9 | Pending PR publication | #34 | Claude fixture-backed child capture, independent sources, reader availability and recovery regressions |
| P8 | A10 | [#29](https://github.com/wangjohn/agent-skills/pull/29) | #28 | Permission-aware privacy evidence and provider guidance |

Review prerequisite PRs #24 → #25 → #26 → #27 first. The earlier #22 retention and #23 release changes remain independent prerequisites for combined release validation. New merge order: #28, then #29 and #30; #31 follows #30; #33 and #32 follow #31; #34 follows #32; P7 follows #34. Retarget descendants as bases merge. No PR has been merged by this task.

## Findings that remain limited

- **A2 / Cursor:** a first-seen `sessionStart` lacks verified fresh-versus-resumed provenance. New ambiguous sessions are not archived. Restoring capture requires version-specific synthetic new/resume evidence and a reliable native start contract; knowing an installed version alone is insufficient.
- **A4 / application support:** setup observes installed versions and reports documented/synthetic evidence separately from publication read-back. This is not a certified compatibility matrix for actual installed app versions.
- **A7 / eligible but unused:** no supported native producer proves complete eligibility and use-observation coverage for the comparison interval. Inventory stays `installed_only`; discovery without use coverage stays partial. No-use queries exclude unknown coverage and unsafe legacy inferences. This required product capability remains blocked, not complete.
- **A9 / subagents:** Claude child capture is implemented for the fixture-backed identity/timestamp contract. Codex/Cursor child formats remain unavailable. Missing IDs, unknown timestamps, or children predating the accepted parent are not captured. Child-before-parent arrival requires a later stop event. Actual app capture is unverified.
- **A10 / privacy:** R2 object credentials do not inspect Cloudflare-managed public domains. R2 remains `not_verified` with provider guidance. AWS checks are bounded and use existing permissions; incomplete/denied checks remain unknown. Private status covers native bucket public controls, not downstream services or signed URLs.

## Live acceptance procedure

Use synthetic prompts and a dedicated private test prefix. Never copy real conversations into test fixtures. Record exact CLI commit, binary checksum, macOS/CPU, installed app version, observed model, harness, scenario, time, expected/actual result, and artifact location. Keep private configuration and credentials outside this public repository.

1. **R2:** configure the test bucket with `agent-archive setup`; verify one-prefix write/read/list/delete and probe cleanup. Start a new synthetic supported app session in an included project. Let scheduled collection run, then check `status --json`, `list`, and `show <id> --normalized`. Compare independent source/metadata read-back. Exercise interruption, offline retry, and denied cleanup. Repeat from launchd to test background credential access. No local test bucket/config was supplied during this task; live R2 is pending.
2. **App/project coverage:** repeat for every selected app/project pair. Add a project and check that only the added pair needs verification. Test resume of a known accepted session and an unknown old session. Verify rejected old sessions produce no source upload. Test delayed final output and requested/response model changes. Unsupported Cursor start and native eligibility must not turn green from hook installation alone.
3. **macOS operation:** test Keychain with the delivered executable, launchd install/restart/login, pause during a pending pass, offline resume, uninstall/reinstall, and legacy migration. Confirm unrelated hooks survive. Generated plist/transaction tests do not substitute for these checks.
4. **Two Macs:** use distinct machine IDs against the same destination; verify independent session ownership, combined listing, offline recovery, and no cross-machine overwrite or deletion.
5. **Release:** build both architectures, sign/notarize with authorized Apple credentials, download actual release artifacts, verify checksums, and launch on Intel and ARM. Cross-compilation alone does not validate ARM execution or Gatekeeper behavior.
6. **Performance:** measure synthetic hook subprocess p95 and separately compare actual enabled/disabled tasks, CPU, memory, and I/O for representative session sizes. A small local hook benchmark does not establish zero task overhead.
7. **AWS:** explicitly deferred by the user. When available, run the storage matrix with explicit profile, SSO refresh/expiry, least-privilege permissions, and background credential behavior.

The separately requested Astra review task did not become available to this run. Root reviewed implementation diffs and corrected defects; this record does not claim that separate review completed. Evaluate Skill itself remains outside these eight remediation slices.
