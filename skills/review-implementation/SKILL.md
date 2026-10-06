---
name: review-implementation
description: Review an implementation against its plan across branches, PRs, or worktrees. Use after implementation to find missing requirements, excess scope, duplicated capabilities, integration gaps, and architectural or performance problems in the combined result.
license: MIT
---

# Review an implementation against its plan

Establish whether the intended outcome was delivered, whether substantial additions have a reason to exist, and whether the combined implementation forms a coherent system. Review in both directions: plan to code for missing work, and code to plan for excess work. Passing tests and agent completion reports are evidence to investigate, not proof of fulfillment.

Default to a review with proposed corrections. Do not change implementation code or the plan, start repair agents, publish comments, or merge changes unless the user requests those actions. Local inspection and appropriate checks are part of the review; use isolated scratch resources where checks generate files or require combining branches.

## Establish intent and the reviewed state

Accept a committed plan, an untracked or temporary file, or a plan supplied in the conversation. Do not require the plan to be committed or moved into the repository. Resolve the implementation target from the request and repository context: a branch, PR set, worktree, or explicitly selected local changes. Ask only when ambiguity materially changes the review; continue independent inspection while awaiting clarification.

Record enough identity to reproduce the review:

- Plan source and version, including a commit ID when applicable. Preserve a review copy or content hash for mutable files, outside tracked source.
- Original requirements, explicit constraints and non-goals, and user-approved amendments. Separate these from implementation checkboxes, agent reports, and proposals to change scope.
- Starting base, relevant merge bases, reviewed head commits, and the relationship and order of component branches or PRs. Compare against the actual starting state; a default branch or last commit alone may omit earlier implementation work or include unrelated work.
- In-scope staged, unstaged, and untracked files when reviewing a worktree. A clean HEAD checkout does not capture those changes. Preserve the user's files and exclude unrelated local work.

If the plan was edited during implementation, inspect available history to distinguish approved changes in intent from changes in reported progress. Do not assume the latest checklist supersedes original requirements or that a worker's explanation authorizes a departure. If the original plan, base, amendment authority, or component state cannot be established, state the uncertainty and qualify affected conclusions instead of guessing.

Review one captured state. If files or refs move during the review, identify which evidence became stale; refresh affected checks or explicitly report the earlier snapshot. Do not imply findings apply to unreviewed changes.

## Map requirements to behavior

Extract substantive requirements at a useful level of granularity. Include acceptance criteria, constraints, non-goals, and necessary end-to-end behavior. Assign stable local identifiers when the plan has none. Keep explicit requirements separate from inferred necessities and suggestions; do not turn every implementation sketch into a mandatory design choice.

For each requirement, trace the actual execution path through relevant callers, configuration, state changes, and observable results. Locate verification evidence and assess what it proves. An unused helper, unregistered handler, mocked integration, or passing unit test does not establish that the feature works end to end.

Assign a status with evidence:

- **Satisfied:** The implementation supports the requirement with appropriate verification. State verification limits.
- **Partial:** Some required behavior or integration is missing.
- **Missing:** The required behavior is absent after inspecting the plausible implementation paths.
- **Intentionally changed:** A documented, authorized amendment changes the requirement; cite it and assess the replacement behavior too.
- **Unverified:** Available code, execution evidence, or context is insufficient to establish fulfillment. Say what would resolve it.

A different implementation can satisfy the same requirement. Evaluate equivalent outcomes against the actual constraints rather than demanding literal adherence to a suggested approach. Report unexplained departures, ambiguous requirements, and flaws in the plan separately; never silently rewrite the plan to match the code.

## Map substantial changes back to purpose

Account for each substantial changed area, grouping related files by behavior rather than producing a file-by-file checklist. Map it to a requirement, necessary supporting work, an authorized amendment, or unexplained scope. Supporting work need not have been named in the plan, but its necessity should be concrete.

Search the surrounding repository for existing capabilities before concluding that a new subsystem is needed or duplicated. Inspect both implementations and their callers. Look for competing sources of truth, alternative execution paths, replacement code that leaves the old path active, unnecessary dependencies, speculative extension points, and refactors whose reach exceeds the task.

Distinguish accidental duplication from an intentional migration or compatibility path. Check migration ownership, routing, and retirement conditions where relevant. File count and diff size alone do not establish overengineering. Recommend removing or consolidating code when that is the smallest sound correction.

## Assess the combined system

Inspect changed code together with the existing behavior it affects. For multiple agents or PRs, examine cross-component contracts and the combined result; individually passing PRs do not establish integration. Use an existing combined checkout or an isolated integration snapshot where feasible, recording exact component commits and target state. Do not alter the user's checkout to assemble it. If combination is unavailable or conflicts, report the integration gap and still complete feasible review.

Apply these lenses in proportion to the actual change. Do not manufacture a finding for every category:

- **Architecture and maintainability:** Ownership of data and behavior, dependency direction, module boundaries, consistency with repository conventions, new coupling, and complexity relative to the task. Follow consequential state and control flows. Explain a concrete maintenance or correctness consequence before proposing a redesign.
- **Integration and compatibility:** Reachability, registration, configuration, callers, shared schemas, defaults, API compatibility, persistence formats, and agreements between components. Check that new behavior survives the real entry point rather than only a test harness.
- **Correctness and resilience:** Failure paths, partial completion, retries, cancellation, concurrency, idempotency, cleanup, and regressions in existing behavior. Prioritize conditions introduced or exposed by this implementation.
- **Performance and resources:** Additional round trips, repeated computation, database access patterns, unbounded collections or queues, retained memory, resource lifetime, contention, and amplification under concurrency. Inspect hot paths and how work grows with input size or load.
- **Verification quality:** Whether checks demonstrate requirements and important failure cases; whether mocks hide broken contracts; whether tests assert intended behavior rather than restate implementation. Reuse checks that cover the reviewed state and run targeted checks to resolve concrete uncertainty.
- **Operations and data safety:** Relevant migrations, rollout order, compatibility during deployment, rollback and recovery, diagnostics, authorization boundaries, isolation, validation, and risks of lost, duplicated, or exposed data.

For performance, distinguish measured regressions from code-supported scaling risks and hypotheses needing a benchmark. Use the plan's workload, budget, and resource constraints when present. Compare baseline and implementation under comparable conditions when measuring. Without a workload or target, explain the uncertainty; do not invent a latency budget, claim production scalability from unit tests, or demand speculative optimization. Avoid expensive or production-impacting checks without authorization.

Separate new defects, pre-existing issues exposed by the change, and unrelated existing problems. A pre-existing issue can still prevent a requirement from working; explain that dependency. Keep unrelated cleanup out of the required corrections.

## Reconcile findings with evidence

Keep severity and confidence separate. Each supported finding needs a concrete trigger or condition, consequence, linked code evidence, the affected requirement or scope concern where applicable, and the smallest recommended correction. For a missing implementation, cite the requirement and the inspected entry points or integration boundary rather than inventing a missing-code line reference.

Distinguish demonstrated defects, evidence-backed risks needing verification, and design choices requiring judgment. Useful design feedback explains what became harder or riskier and why the proposed change improves it. Do not present stylistic preferences or a possible alternative architecture as defects.

Deduplicate findings with the same root cause and check plausible counterevidence. If existing reviews cover the same snapshot, reuse their evidence and verify relevant dispositions without treating their verdict as authoritative. This review remains responsible for requirement coverage and the combined result; it does not require another skill or a repeated full PR review.

When the user requests independent reviewers, give fresh reviewers the same factual snapshot and bounded responsibilities, such as requirements and scope, architecture and integration, or runtime risks. Withhold implementer verdicts and other reviewers' conclusions until reconciliation. Disclose unavailable isolation or incomplete passes; do not claim independent review from role changes inside one context.

## Deliver an actionable report

Start with a short assessment of requirement fulfillment, consequential scope drift, and the most important issue or decision. Qualify the assessment when material behavior remains unverified. Do not use a quality score or percentage complete that obscures critical gaps.

Include these elements, scaling detail to the implementation:

1. **Reviewed state:** Plan identity, base and implementation snapshots, included local changes, and component or integration state.
2. **Requirement map:** Requirement, status, implementation evidence, verification evidence, and unresolved gap. Keep every substantive requirement represented; group only where the status and evidence remain clear.
3. **Scope accounting:** Substantial changed areas and their purpose, highlighting unexplained additions, duplication, or authorized departures. Necessary supporting work should not be labeled scope drift merely because the plan omitted its mechanics.
4. **Findings and corrections:** Prioritized supported problems with consequence, evidence, and proposed correction. Clearly label concerns that need verification.
5. **Design decisions:** Consequential tradeoffs, plan ambiguities, or proposed amendments needing user judgment, separated from demonstrated defects.
6. **Coverage and next actions:** Checks actually run and their outcomes, skipped or unavailable checks, unreviewed areas, integration limits, and the specific evidence or decision needed to close material gaps.

Keep empty sections brief. Link to verified code locations and plan requirements. If a detailed report is saved, keep it outside tracked source unless the user requests a repository artifact, and provide its location along with the assessment in chat. A review can be complete while finding the implementation incomplete; do not repair it silently or imply that review proves the absence of bugs.
