---
name: review-implementation
description: Review the combined implementation against its plan after coding or agent execution. Find missing requirements, excess scope, duplicated capabilities, and consequential integration, architecture, or performance problems across branches, PRs, or worktrees.
license: MIT
---

# Review an implementation against its plan

Determine whether the intended outcome was delivered, whether substantial additions have a reason to exist, and whether the resulting system is sound. Review in both directions: plan to code for missing work, and code to plan for excess work.

Review and recommend corrections; change code or the plan only when requested. Choose the investigation and checks appropriate to the change rather than following a fixed sequence.

## Establish the basis of the review

Accept plans from repository files, temporary or untracked files, or the conversation. Resolve the plan, implementation scope, and starting state from available context; ask when ambiguity would materially change the conclusions. Include relevant local changes and the full implementation, not merely its latest commit. Identify the reviewed state well enough that findings can be tied to it, and disclose material uncertainty or subsequent changes.

Treat requirements, constraints, non-goals, and approved amendments as intent. Completion checkboxes and agent reports are claims to verify. When a plan has evolved, distinguish changes in intent from progress updates using available evidence. Do not silently reinterpret the plan to make the implementation pass. An alternative approach can satisfy the plan; an unexplained departure or a flaw in the plan may need a human decision.

## Investigate the implementation

Account for every substantive requirement and substantial changed area during the review. Keep this working analysis as detailed as necessary; it need not become the final report.

- **Fulfillment:** Trace requirements to actual behavior and meaningful verification. Distinguish fulfilled, partial, missing, changed, and unverified outcomes. Code that exists but is not connected to the real execution path does not fulfill a requirement.
- **Scope and duplication:** Connect substantial additions to requirements or necessary supporting work. Search existing capabilities and their callers before judging a new subsystem. Look for competing implementations, unnecessary abstractions, and unrelated expansion, while recognizing legitimate migrations and compatibility paths. Diff size alone does not establish overengineering.
- **Architecture and integration:** Assess the combined system and affected existing code, including ownership, boundaries, dependencies, and cross-component contracts. Individually sound PRs can fail together. Identify design changes warranted by what implementation revealed, with concrete benefits rather than stylistic preferences.
- **Performance and resources:** Examine consequential paths for how latency, memory, resource use, and contention grow with workload. Distinguish measured regressions, code-supported scaling risks, and hypotheses needing measurement. Use known workloads and constraints; do not invent budgets or recommend speculative optimization.
- **Correctness and delivery:** Investigate relevant regressions, failure handling, compatibility, security, data integrity, rollout, and recovery. Assess whether tests and other evidence establish the intended behavior, including important integration and failure cases.

Follow the risks of the actual change; these are lenses, not quotas for findings. Use targeted checks to resolve uncertainty and reuse applicable evidence. If the combined implementation cannot be inspected or exercised, explain what remains unverified. Preserve the user's working state when assembling or testing changes.

## Exercise judgment

Ground findings in a concrete consequence and code or behavioral evidence. Check plausible counterevidence and consolidate issues with the same root cause. Distinguish demonstrated defects, credible risks needing verification, and design choices requiring judgment; confidence and impact are different dimensions.

Recommend the smallest sound correction, including removing or consolidating code when appropriate. Necessary supporting work is not scope drift merely because the plan omitted its mechanics. Separate issues introduced or exposed by the implementation from unrelated existing problems; an existing issue still matters when it prevents the requested outcome.

Passing tests and prior reviewer verdicts do not establish plan fulfillment. Conversely, unavailable verification is not proof of a defect. State what evidence or decision would resolve material uncertainty.

## Make the result easy to act on

Lead with a short assessment: whether the intended outcome is implemented, whether consequential scope or design concerns remain, and what deserves attention first. Qualify conclusions where evidence is incomplete.

Put actionable findings next, ordered by importance. For each, explain **the problem, why it matters, the supporting evidence, and the recommended action**. Link relevant code and plan locations; for missing behavior, cite the requirement and inspected boundary. Separate decisions needing human judgment from fixes supported by evidence. Avoid repeating a finding across categories.

Close with a compact coverage note identifying the plan and implementation reviewed, meaningful checks and outcomes, and any limits that affect the assessment. Summarize satisfied requirements together; highlight missing, partial, changed, or unverified requirements that matter. Include a requirement table only when it makes the result easier to understand or the user requests it.

Aim for a report a human can scan in a few minutes. Omit empty sections, routine investigation history, exhaustive file inventories, and numeric quality scores. Keep all consequential findings visible; when detail is extensive, summarize the actions in chat and link a fuller report. Do not sacrifice evidence or hide unresolved requirements to meet a length target. If there are no actionable findings, say so plainly with the material coverage limits.
