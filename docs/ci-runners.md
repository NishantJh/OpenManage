# CI runners

Trusted branch-push CI runs on self-hosted runners. The dedicated Ubuntu VM runner carries the labels `self-hosted, Linux, X64, openmanage-ci`. The Windows PC WSL2 runner remains available for the existing main-branch CI workflow until the CI migration is completed.

## Security rules

- This is a public repository. Never execute code from untrusted fork pull requests on a self-hosted runner. Do not use `pull_request_target` to check out or execute PR code. Self-hosted PR validation must be limited to trusted repository collaborators and must check the PR head repository and author association before checking out code.
- Keep runner service accounts unprivileged. Do not grant blanket passwordless sudo, membership in the `sudo` group, or passwordless access to general-purpose `ip`, `iptables`, `sysctl`, `chmod`, or shell commands.
- Keep the self-hosted runner dedicated to trusted OpenManage workflows. Do not run untrusted projects or arbitrary workloads on the same account.
- Use workflow concurrency to avoid overlapping tests that share host ports, Docker resources, or the fixed QEMU TAP interface.

## Ubuntu runner prerequisites

Install the non-secret system dependencies once, interactively as an administrator:

```bash
sudo apt-get update
sudo apt-get install -y qemu-system-x86 qemu-utils iproute2 iptables
```

The Docker Engine and Compose plugin must already be installed, and the `runner` account must be able to use Docker without sudo. The VM flash test downloads official OpenWrt x86-64 images and requires outbound internet access.

## Restricted privilege helper for the QEMU VM test

The VM flash test needs one TAP interface, a temporary IPv4 forwarding setting, and three narrowly scoped firewall rules. It must not receive general root access. The repository helper `scripts/openmanage-vm-net-helper.sh` implements only two fixed operations: `setup` and `cleanup`. It hard-codes the reserved interface `om-vmtap`, host address `192.168.1.2/24`, and firewall rules; it accepts no caller-supplied command or network parameters. It saves and restores the prior `net.ipv4.ip_forward` value in `/run`.

After the helper change is available on the trusted branch, install it from the repository checkout as an administrator:

```bash
sudo install -o root -g root -m 0755 \
  scripts/openwrt-vm-net-helper.sh \
  /usr/local/sbin/openmanage-vm-net
```

Create the exact-command sudoers rule. This grants the `runner` account permission to invoke only the helper's two fixed subcommands:

```bash
printf '%s\\n' \
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

## Ephemeral runner lifecycle

An ephemeral runner accepts one job and then deregisters. It does not automatically provision a replacement. If multiple workflows/jobs target the same single manually registered runner, re-register it before the next queued job can run. Prefer a consolidated workflow or a managed runner pool if several checks must run unattended.

## Existing WSL2 runner

The Windows PC WSL2 runner remains separately labelled `self-hosted, Linux, X64, WSL2`. Chromium system libraries are installed on the Ubuntu VM, but the WSL2 runner lacks them; browser tests should only be skipped on WSL2, not on the Ubuntu VM.
