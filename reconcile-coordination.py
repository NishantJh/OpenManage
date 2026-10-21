import base64
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

API = "https://api.github.com"
CONFIG = Path(__file__).with_name("coordination-workstreams.json")
PATTERNS = (
    re.compile(r"(?im)^\s*-?\s*Target PR head SHA observed:.*?([0-9a-f]{40})"),
    re.compile(r"(?im)^\s*-?\s*Head:.*?([0-9a-f]{40})"),
)


def get_json(path):
    headers = {
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "OpenManage-Coordinator-Reconciler",
    }
    token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    if token:
        headers["Authorization"] = "Bearer " + token
    request = urllib.request.Request(API + path, headers=headers)
    with urllib.request.urlopen(request, timeout=15) as response:
        return json.load(response)


def extract_handover_head(encoded_content):
    text = base64.b64decode(encoded_content).decode("utf-8")
    for pattern in PATTERNS:
        match = pattern.search(text)
        if match:
            return match.group(1).lower()
    return None


def main():
    repository = os.environ.get("GITHUB_REPOSITORY", "")
    if "/" not in repository:
        print("GITHUB_REPOSITORY must be owner/repository", file=sys.stderr)
        return 2
    try:
        config = json.loads(CONFIG.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        print("Could not read workstream registry: " + str(exc), file=sys.stderr)
        return 2

    lines = [
        "# OpenManage coordinator reconciliation", "",
        "Repository: " + repository, "",
        "Read-only snapshot: this does not dispatch agents, approve PRs, or merge code.", "",
        "| Workstream | PR state | Current head | Handover head | Result |",
        "|---|---|---|---|---|",
    ]
    attention = 0
    for item in config.get("workstreams", []):
        label = item["id"] + " — " + item["title"]
        try:
            pr = get_json("/repos/" + repository + "/pulls/" + str(int(item["pull_request"])))
            current = (pr.get("head") or {}).get("sha", "").lower()
            ref = urllib.parse.quote(item["handover_ref"], safe="")
            path = urllib.parse.quote(item["handover_path"], safe="/")
            blob = get_json("/repos/" + repository + "/contents/" + path + "?ref=" + ref)
            recorded = extract_handover_head(blob["content"])
            state = "merged" if pr.get("merged") else pr.get("state", "unknown")
            if pr.get("draft"):
                state += " (draft)"
            if state == "merged":
                result = "Review post-merge evidence"
            elif not recorded:
                result = "STALE/INCOMPLETE: target SHA missing"
                attention += 1
            elif recorded != current:
                result = "STALE: handover differs from PR head"
                attention += 1
            else:
                result = "Head matches; inspect CI and acceptance evidence"
            lines.append(
                "| " + label + " | " + state + " | " + current[:12] +
                " | " + (recorded or "not found")[:12] + " | " + result + " |"
            )
        except (urllib.error.URLError, urllib.error.HTTPError, KeyError, ValueError, TypeError) as exc:
            attention += 1
            lines.append("| " + label + " | unknown | unknown | unknown | ERROR: " +
                         str(exc).replace("|", "/") + " |")

    lines.extend([
        "", "## Coordinator actions", "",
        "- Verify exact-head CI and required regression tests; SHA agreement alone is not approval.",
        "- Ask owners to refresh stale handovers.",
        "- Keep merge and production-impacting actions behind explicit human approval.", "",
    ])
    report = "\n".join(lines)
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as output:
            output.write(report + "\n")
    else:
        print(report)
    print("Reconciliation complete; workstreams needing attention: " + str(attention))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
