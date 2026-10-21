# Coordinator infrastructure

## Purpose

OpenManage uses parallel workstreams, but separate chat conversations do not share live state or provide a supported way for this repository to send messages into another ChatGPT thread. This infrastructure makes GitHub the durable coordination control plane and automates stale-handover detection without pretending that repository automation can message a chat directly.

## MVP components

- `.github/coordination/workstreams.json` registers each tracked workstream, its target PR, canonical handover ref/path, and owner.
- `scripts/coordination/reconcile.py` reads the configured PRs and handovers through the GitHub API and compares the handover's recorded target head SHA with the live PR head SHA.
- `.github/workflows/coordination-reconcile.yml` runs every 15 minutes and on manual dispatch. Its run summary is the current reconciliation report.
- The workflow is read-only: it cannot write issues, push branches, approve/merge PRs, or modify production systems. A stale/missing handover is reported for coordinator action; a matching SHA is not treated as proof that CI passed.

## Operating procedure

1. Every agent works on an isolated branch and owns a defined workstream.
2. The owner updates the canonical handover after meaningful progress. The handover must record the target PR's current head SHA, status, evidence URLs, blockers, decision needed, and next action.
3. The reconciliation workflow compares that recorded target SHA with GitHub's current PR head. Run it manually from Actions when an immediate check is needed.
4. The coordinator reviews the workflow summary, inspects the exact-head CI runs and changed-file overlap, then assigns or approves the next action.
5. Keep merge approval and production-impacting actions human-controlled. Never include credentials or sensitive logs in handovers.

## Handover format

Use a stable Markdown field for the target PR head, for example:

    Target PR head SHA observed: 0123456789abcdef0123456789abcdef01234567

The reconciler also recognizes the `Head: <40-hex-sha>` field used by workstream handovers. The SHA must refer to the target PR's code head, not the commit that only updates the handover document.

## Current limitation: agent dispatch

This MVP detects stale state; it does not launch or message agents. GitHub does not provide an API to send a prompt into an arbitrary ChatGPT conversation. True dispatch requires a supported external agent runtime or bridge with authenticated worker endpoints. Such a bridge must be added as a separate, reviewed integration, with credentials stored in GitHub Actions secrets, explicit allowlisted actions, idempotency, audit logging, and a human-approval boundary. Do not store agent/API secrets in this repository or grant the reconciler write permissions just to simulate dispatch.

## Security and operations

- Runs on GitHub-hosted `ubuntu-latest`, not the self-hosted WSL2 runner.
- Uses only read permissions and the workflow-provided token; no additional secrets are required.
- No app runtime service, database migration, dependency, or default Compose service is added.
- Scheduled GitHub Actions execution is best-effort and may be delayed or disabled by repository policy. Use manual dispatch for an immediate snapshot.
- Review workflow run summaries as reports, not merge authorization.
