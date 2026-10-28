# CI runners

Trusted branch-push CI (`.github/workflows/ci.yml`) runs on self-hosted runners. Two hosts carry the same labels (`self-hosted, Linux, X64, WSL2`), so whichever is online picks up the job:

- The Windows PC: WSL2 Ubuntu-24.04 runners using Docker Desktop. Docker Desktop must be running.
- The Ubuntu VM (LAN 192.168.1.180): the runner host for all of Nishant's projects. The runner is a systemd service under the non-root user `runner`, with native Docker Engine. It also carries the extra label `vm`.

## Rules

- Self-hosted runners are trusted infrastructure on a public repo. Never add `pull_request` or `pull_request_target` jobs for them; pull requests run on GitHub-hosted runners (`pr-validation.yml`).
- Gate self-hosted jobs to `refs/heads/main` so `workflow_dispatch` cannot run another ref there.
- The VM is shared by several repos: use no fixed container names or host ports in workflows. A weekly `docker system prune` timer cleans the shared Docker daemon.
- Only repos Nishant owns get a runner on the VM.

## Adding a runner on the VM for another repo

1. Create a token at the repo's Settings > Actions > Runners > New self-hosted runner.
2. On the VM as `nishant`: `add-runner OWNER/REPO self-hosted,Linux,X64,vm` and paste the token at the prompt. The token is never stored. The runner package is cached in `/opt/actions-runner-cache` (GitHub's download server is slow per connection).
3. Each repo gets its own directory under `/home/runner/` and its own systemd unit `actions.runner.*`.

Chromium system libraries for Playwright are installed on the VM, so the browser step of `scripts/smoke-test.sh` can run there.
