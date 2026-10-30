# CI runners

The dedicated Ubuntu VM is the persistent runner for trusted main-branch CI, scheduled security analysis, dependency generation, and opt-in OpenWrt lab tests. Its labels are `self-hosted, Linux, X64, openmanage-ci`. The Windows PC WSL2 runner is no longer targeted by repository workflows.

## Security rules

- This is a public repository. Pull-request validation, CodeQL, and dependency security checks for PRs remain on GitHub-hosted runners. A PR can modify its workflow files, so a conditional inside a `pull_request` workflow is not a sufficient security boundary for placing a public-repository PR on a self-hosted runner.
- Self-hosted workflows execute only trusted code from `main`. Any `workflow_dispatch` job targeting this runner must reject refs other than `refs/heads/main`.
- Do not use `pull_request_target` to check out or execute PR code.
- Keep runner service accounts unprivileged. Do not grant blanket passwordless sudo, membership in the `sudo` group, or passwordless access to general-purpose `ip`, `iptables`, `sysctl`, `chmod`, or shell commands.
- Keep the runner dedicated to trusted OpenManage workflows. Do not run untrusted projects or arbitrary workloads on the same account.
- The runner executes one job at a time. GitHub queues additional jobs until it is free. OpenWrt lab and QEMU VM tests also share a concurrency group so their host networking resources cannot overlap.

## Ubuntu runner prerequisites

Install the non-secret system dependencies once, interactively as an administrator:

```bash
sudo apt-get update
sudo apt-get install -y qemu-system-x86 qemu-utils iproute2 iptables eapoltest
```

The Docker Engine and Compose plugin must already be installed, and the `runner` account must be able to use Docker without sudo. The VM flash test downloads official OpenWrt x86-64 images and requires outbound internet access. Chromium system libraries are required because the main-branch smoke test runs its browser navigation check.

## Configure the runner as a persistent service

The current registration was created as ephemeral. Replace that registration with a normal persistent registration; do not use the `--ephemeral` flag.

1. Stop and uninstall the existing runner service if one was installed:

   ```bash
   sudo ./svc.sh stop
   sudo ./svc.sh uninstall
   ```

   If no service was installed, skip these two commands.

2. From the runner installation directory, remove the old registration as the runner account. Use a fresh removal token if GitHub requests one:

   ```bash
   ./config.sh remove
   ```

3. In GitHub, open **Settings → Actions → Runners → New self-hosted runner** and generate a fresh registration token. Configure the runner from its installation directory as the runner account:

   ```bash
   ./config.sh --url https://github.com/NishantJh/OpenManage --token YOUR_FRESH_TOKEN --labels openmanage-ci --unattended
   ```

   Do not add `--ephemeral`. The default runner name and labels `self-hosted`, `Linux`, and `X64` are retained; the command adds `openmanage-ci`.

4. Install and start the service so it survives logout and host restart:

   ```bash
   sudo ./svc.sh install runner
   sudo ./svc.sh start
   sudo ./svc.sh status
   ```

5. Confirm the runner appears as **Idle** under GitHub **Settings → Actions → Runners**. It should remain online after completing a job instead of deregistering.

Keep the runner software updated. Restrict repository access to this dedicated runner, and do not use it for untrusted repositories or workflows.

## Restricted privilege helper for the QEMU VM test

The VM flash test needs one TAP interface, a temporary IPv4 forwarding setting, and three narrowly scoped firewall rules. It must not receive general root access. The repository helper `scripts/openmanage-vm-net-helper.sh` implements only two fixed operations: `setup` and `cleanup`. It hard-codes the reserved interface `om-vmtap`, host address `192.168.1.2/24`, and firewall rules; it accepts no caller-supplied command or network parameters. It saves and restores the prior `net.ipv4.ip_forward` value in `/run`.

After the helper change is available on the trusted branch, install it from the repository checkout as an administrator:

```bash
sudo install -o root -g root -m 0755 \
  scripts/openmanage-vm-net-helper.sh \
  /usr/local/sbin/openmanage-vm-net
```

Create the exact-command sudoers rule. This grants the `runner` account permission to invoke only the helper's two fixed subcommands:

```bash
printf '%s\n' \
  'runner ALL=(root) NOPASSWD: /usr/local/sbin/openmanage-vm-net setup, /usr/local/sbin/openmanage-vm-net cleanup' \
  | sudo tee /etc/sudoers.d/openmanage-vm-net >/dev/null
sudo chown root:root /etc/sudoers.d/openmanage-vm-net
sudo chmod 0440 /etc/sudoers.d/openmanage-vm-net
sudo visudo -cf /etc/sudoers.d/openmanage-vm-net
```

Verify that the runner account is authorized without executing either network operation:

```bash
sudo -u runner sudo -n -l /usr/local/sbin/openmanage-vm-net setup
sudo -u runner sudo -n -l /usr/local/sbin/openmanage-vm-net cleanup
```

Both commands must list the corresponding command as permitted without asking for a password. Do not test by running `setup` manually; the workflow owns setup and cleanup as a pair. The script installs an EXIT cleanup trap, and the helper also removes partial setup if a setup command fails.

If the runner account is not literally named `runner`, change the fixed `RUNNER_USER` in the helper and the username in the sudoers rule to the actual service account before installing it. Keep the sudoers command paths and arguments exact.

The helper intentionally does not change `/dev/kvm` permissions. QEMU uses KVM only if the runner already has access; otherwise it falls back to TCG emulation. If KVM performance is needed, grant the runner account membership in the host's `kvm` group through normal host administration, then restart the runner service. Do not make `/dev/kvm` world-writable.

## Runner behavior and workflow placement

- `.github/workflows/ci.yml`, trusted main-branch CodeQL/security scans, dependency-lockfile generation, and the opt-in OpenWrt lab/VM tests target `openmanage-ci`.
- PR validation and PR-triggered CodeQL/security checks remain GitHub-hosted to avoid running potentially modified PR workflow definitions on a persistent runner.
- The dependency-lockfile publish job and image publishing remain GitHub-hosted. They are narrowly scoped publication jobs; the lockfile publish job does not install dependencies or execute project build/test scripts.
- The OpenWrt lab and QEMU VM workflows share a concurrency group. Do not remove that lock while both tests manipulate host networking.
