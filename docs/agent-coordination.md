# Agent coordination protocol

## Purpose and limitations

Use GitHub files as a durable message bus between agents working in separate ChatGPT conversations. This protocol does not itself wake up another chat or grant cross-chat access. Agents must commit/push their notes; the coordinator discovers them during a repository reconciliation. For unattended checks, a separate scheduled agent/automation with repository access is required.

## Shared files

- handoffs/coordinator-inbox.md: coordinator-owned status, decisions, assignments, and requests to agents. Append dated updates; do not rewrite history.
- handoffs/pr-21-radius.md: Agent B's canonical handover for the optional FreeRADIUS lab.
- handoffs/pr-26-wifi-layout.md: Agent A's canonical handover for Wi-Fi Layout.
- handoffs/mvp-test-harness.md: Agent C's plan and test evidence.
- Other workstreams should use handoffs/<workstream>.md, one canonical file per workstream.
- plan.md: coordinator-maintained roadmap and priorities, not a substitute for detailed handovers.

## Handover format

Every update should include:

1. Updated: ISO-8601 UTC timestamp.
2. Owner / workstream: agent label and assigned scope.
3. Status: not-started, in-progress, blocked, ready-for-review, or complete.
4. GitHub: PR URL, branch, head SHA, base SHA if known.
5. Verified evidence: exact commands/check names, run URLs, commit SHA, and pass/fail/pending result. Clearly label untested statements as assumptions.
6. Changes: files changed and links to commits/PRs.
7. Findings: severity, impact, reproduction steps, and supporting evidence.
8. Blockers / dependencies: what is needed, from whom, and why.
9. Coordinator decision requested: a specific question or none.
10. Next action: one owner and a concrete next step.

Append a dated entry for each material update. Never put secrets or raw sensitive logs in these files.

## Agent message semantics

- REPORT: factual status/evidence; no response required unless a blocker exists.
- REQUEST: asks the coordinator for a decision or another workstream's action. Include the decision deadline only if real.
- BLOCKED: work cannot safely proceed; state the precise dependency.
- READY_FOR_REVIEW: work is complete enough for independent review; list evidence and remaining limitations.
- DECISION: coordinator records a decision, rationale, owner, and any conditions.

These labels are plain headings in Markdown, not an automated notification system.

## Coordinator reconciliation loop

When reconciling the repository, the coordinator should:

1. Read the inbox and all active workstream handovers.
2. Check each cited PR's actual state, head/base SHA, review threads, and CI on the current head.
3. Reject stale reports where the branch/commit has moved; ask the owner to refresh evidence.
4. Resolve dependencies and file ownership conflicts before authorizing overlapping edits.
5. Write decisions and assignments to the inbox and update the relevant handover/plan.
6. Never merge or close PRs, change production configuration, or authorize production-device testing without explicit user approval.
7. Report only actionable changes to the user; do not treat a closed PR as merged unless GitHub confirms it.

## Concurrency rules

- One owner per PR/workstream and one canonical handover file.
- Agents may independently inspect shared code, but must announce file overlap before editing.
- Avoid multiple agents editing AGENTS.md, plan.md, the same migration, or the same workflow concurrently. The coordinator owns cross-workstream integration edits.
- Agent handover updates should normally be made on the agent's own branch and included in that agent's PR. Do not push directly to main.
- If a handover file does not yet exist on the agent's branch, add it there and include it in the existing PR when scope permits. Otherwise open a small docs-only PR and link it from the inbox.

## Initial active workstreams (2026-10-21)

- PR #28 handler: owns dependency-lockfile consolidation. Finish and validate #28 before reconciling/closing #18 and #23.
- Agent A: owns validation of PR #26 only.
- Agent B: owns validation of PR #21 only.
- Agent C: prepares the MVP disposable-lab test plan; no repository edits until coordinator approval.
- Coordinator: verifies PR #24 merge state and outstanding runtime validation, manages integration, and owns this inbox.
