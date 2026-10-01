# Run state template

Use this as a starting point for the run's `plan.md`. Keep it concise and update it as facts change. The orchestrator is its only writer; worker reports live alongside it in separate files.

```markdown
# Implementation run

## Goal and acceptance
- User request:
- Acceptance criteria:
- Scope and exclusions:
- Starting commit and any captured in-scope local edits:
- Acceptance criterion / owning PR / verification evidence:
- Decisions and open questions:

## Run settings
- Run ID and state directory:
- Repository and target branch:
- Host capabilities and material limits:
- Merge mode: handoff | merge
- Merge authorization: user instruction and scope, or unanswered
- Changes or revocations to authorization:
- Merge method:
- Concurrency and any user time/cost limits:
- Review/fix limit: 3 per PR; final verification-only pass if needed

## PR plan
| ID | Scope | Depends on | Branch / base | State | PR |
|---|---|---|---|---|---|

States: planned, implementing, reviewing, fixing, verifying,
waiting-checks, technically-ready, awaiting-approval, awaiting-user-merge,
merging, queued, merged, blocked, cancelled.
In handoff mode, awaiting-user-merge can satisfy the run's deliverable.

## Per-PR evidence
### PR <ID>
- Acceptance criteria and expected code areas:
- Cross-PR contracts and test strategy:
- Checkout and branch:
- Base branch, target-tip, merge-base, and dependency heads:
- Implementer ID, status, report, and write ownership:
- Ownership transfer evidence, including background writers stopped:
- PR URL and current head:
- Draft status:
- Reviewer IDs, statuses, reports, and reviewed heads:
- Findings: ID / evidence / disposition / fix commit or blocker
- Review/fix pass count:
- Checks: command or CI URL / outcome / commit or merge candidate
- Technical readiness evidence and remaining conditions:
- Merge eligibility: authorization / approvals / protections / candidate checks
- Merge result, if applicable:

## Integration
- Intended merge order:
- Combined candidate or target commit tested:
- Exact component PR heads and dependency/base commits:
- Checks and results:
- Gaps or blockers:

## Next actions and recovery
- Active workers and their owned resources:
- Next eligible work:
- User decisions needed:
- Recoverable branches, patches, and artifacts:

## Milestones
- Timestamp / event / evidence / resulting decision

## External operations
- Operation / intended target and expected head / outcome or unknown
- Remote-state reconciliation before retry:
```
