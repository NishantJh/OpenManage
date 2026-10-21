#!/usr/bin/env python3
"""Reconcile configured workstream handovers against live GitHub pull requests.

Read-only by design: this script reads GitHub metadata and emits Markdown.
It never edits PRs, issues, branches, or repository files.
"""
from __future__ import annotations

import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

API = "https://api.github.com"
ROOT = Path(__file__).resolve().parents[2]
CONFIG = ROOT / ".github" / "coordination" / "workstreams.json"
HANDOVER_HEAD_PATTERNS = (
    re.compile(r"(?im)^\s*-?\s*Target PR head SHA observed:\s*`?([0-9a-f]{40})`?\s*$"),
    re.compile(r"(?im)^\s*-?\s*Head:\s*`?([0-9a-f]{40})`?\s*$"),
)


class GitHubAPIError(RuntimeError):
    pass


def github_get(path: str) -> Any:
    token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    headers = {
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "OpenManage-Coordinator-Reconciler",
    }
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = urllib.request.Request(API + path, headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            return json.load(response)
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError) as exc:
        raise GitHubAPIError(f"GitHub GET {path} failed: {exc}") from exc


def extract_handover_head(content: str) -> str | None:
    for pattern in HANDOVER_HEAD_PATTERNS:
        match = pattern.search(content)
        if match:
            return match.group(1).lower()
    return None


def reconcile(config: dict[str, Any], repository: str) -> tuple[str, int]:
    lines = [
        "# OpenManage coordinator reconciliation",
        "",
        f"Repository: `{repository}`",
        "",
        "> Read-only snapshot. This report does not dispatch agents, approve PRs, or merge code.",
        "",
        "| Workstream | PR state | Current PR head | Handover head | Reconciliation |",
        "|---|---|---|---|---|",
    ]
    problems = 0
    for item in config.get("workstreams", []):
        label = f"{item['id']} — {item['title']}"
        number = int(item["pull_request"])
        try:
            pr = github_get(f"/repos/{repository}/pulls/{number}")
            state = "merged" if pr.get("merged") else pr.get("state", "unknown")
            if pr.get("draft"):
                state += " (draft)"
            current_head = (pr.get("head") or {}).get("sha", "").lower()
            ref = urllib.parse.quote(item["handover_ref"], safe="")
            path = urllib.parse.quote(item["handover_path"], safe="/")
            try:
                blob = github_get(f"/repos/{repository}/contents/{path}?ref={ref}")
                import base64
                handover_text = base64.b64decode(blob["content"]).decode("utf-8")
                handover_head = extract_handover_head(handover_text)
            except GitHubAPIError as exc:
                handover_head = None
                handover_error = str(exc)
            else:
                handover_error = ""

            if state == "merged":
                result = "PR already merged; review post-merge evidence"
            elif not handover_head:
                result = "STALE/INCOMPLETE: handover head missing"
                if handover_error:
                    result += " (handover unavailable)"
                problems += 1
            elif handover_head != current_head:
                result = "STALE: handover does not match current PR head"
                problems += 1
            else:
                result = "head matches handover; inspect CI and acceptance criteria"
            lines.append(
                f"| {label} | {state} | `{current_head[:12] or 'unknown'}` | "
                f"`{(handover_head or 'not found')[:12]}` | {result} |"
            )
            if handover_error:
                lines.extend(["", f"**{label}:** handover lookup failed: `{handover_error}`", ""])
        except (GitHubAPIError, KeyError, TypeError, ValueError) as exc:
            problems += 1
            lines.append(f"| {label} | unknown | unknown | unknown | ERROR: {exc} |")
    lines.extend([
        "",
        "## Coordinator action required",
        "",
        "- Verify latest-head CI and required regression evidence; a matching SHA alone is not approval.",
        "- Resolve stale handovers with the workstream owner before making readiness decisions.",
        "- Keep merges and production-impacting actions behind explicit human approval.",
        "",
    ])
    return "\n".join(lines), problems


def main() -> int:
    repository = os.environ.get("GITHUB_REPOSITORY")
    if not repository or "/" not in repository:
        print("GITHUB_REPOSITORY must be owner/repository", file=sys.stderr)
        return 2
    try:
        config = json.loads(CONFIG.read_text(encoding="utf-8"))
        report, problems = reconcile(config, repository)
    except (OSError, json.JSONDecodeError, GitHubAPIError) as exc:
        print(f"coordination reconciliation failed: {exc}", file=sys.stderr)
        return 2

    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as summary:
            summary.write(report)
            summary.write("\n")
    else:
        print(report)
    # Findings are reported in the summary; only an actual API/config failure fails the job.
    print(f"Reconciliation complete; workstreams needing attention: {problems}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
