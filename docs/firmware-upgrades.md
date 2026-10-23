# Firmware build and upgrade: design

Status: proposal, not started. Design only; no code. Comes after the current plan (Sections 3 to 8) is finished and tested. It refines plan.md Section 5.

## Goal

For each managed OpenWrt device, build a firmware image that already contains every package installed on that device (including third-party ones such as `luci-theme-fluent`), flash it, and end up with the same configuration, like ASU's "attended sysupgrade" but covering packages ASU cannot know about.

## Feasibility verdict

Feasible, with one hard limit: the part that matches ASU (official packages, official releases) is a modest amount of work. The part that goes beyond ASU (third-party packages, config carried across releases, recovery from a bad flash) is where the effort and risk are. I would not promise "everything survives every upgrade" for the first release.

Hardest parts, in order:

1. **Failed flashes cannot be fixed over SSH.** A bad image or power loss can leave a device unreachable. Single-partition devices have no automatic rollback. The design limits the blast radius (canary, pre-checks, one device at a time) but cannot remove this risk. Matches plan.md Section 5.
2. **Third-party packages.** ASU builds only from official feeds. A package from a custom feed or a hand-installed `.ipk`/`.apk` has to be added another way (see below), and it must be rebuilt or compatible with the target release and architecture.
3. **Configuration across releases.** `sysupgrade -k` restores the old config, but release changes (for example DSA networking, `opkg` to `apk`) can need migration. The backup is secret-bearing, so it must be stored encrypted.
4. **Trust in the build.** Images come from an external build service or a locally run Image Builder, so integrity checks matter more than for a plain file download.
5. **Fitting the "no extra services" rule.** A local Image Builder is big (GB of disk, many build tools) and is a poor fit for the main container.

Overall size estimate: a large feature, best done in the phases below. Each phase is useful alone.

## How a device is described

A **package manifest** is captured per device by typed read operations and stored as a versioned snapshot beside config snapshots:

- OpenWrt version, target/subtarget, `board_name` (from `ubus call system board`), architecture, and package manager (`opkg` up to 24.10, `apk` from 25.x).
- **User-installed packages**, not dependencies: with `opkg`, packages whose status line says `user installed`; with `apk`, the explicit list in `/etc/apk/world`. This avoids pinning every dependency, so the new release can pick compatible dependency versions.
- **Feed origin per package.** Compare the list against the official package index for that release and architecture. Anything not found there is "third-party". Also record configured custom feeds (`customfeeds.conf`, `/etc/apk/repositories.d/`) and, for third-party packages, the version and where it came from if known (feed, or an administrator-recorded URL plus SHA-256).
- Free flash and RAM, so an image that will not fit is rejected early.

These are new typed read operations (names `firmware_inventory`, similar), using fixed command templates. No generic shell.

## Building the image

| Option | Extra service | Third-party packages | Notes |
| --- | --- | --- | --- |
| **A. Public ASU API** (`sysupgrade.openwrt.org`, `POST /api/v1/build`) | None; the Go process calls it | No (official feeds only), but a `defaults` script (uci-defaults) can be included | Easiest. Sends the device model and package list to a third party; needs internet. |
| **B. Local Image Builder** | A builder container or job | Yes: local packages dir or a custom feed | Offline and private, but large and slow. Needs a container runtime or a dedicated builder container. |
| **C. Self-hosted ASU** | Yes (server plus queue) | Limited | Breaks the "no extra services" rule. Not recommended. |
| **D. On-device `owut` / `auc`** | None | No | Quick path for official packages only; the device itself asks ASU. Weak control over verification. |

Recommendation: **A first, B later and optional.** Phase 1 uses ASU for the official part and handles third-party packages after the flash (below). B is added only if Nishant wants fully offline builds or third-party packages inside the image. Build jobs would run as a separate optional builder container, started by Compose profile, never as a required service and never by giving OpenManage access to the Docker socket.

Profile mapping: ASU needs release, target, profile and package list. The profile comes from `board_name` (commas replaced with underscores). To be confirmed in the lab for each supported model.

## Third-party packages

Options, in order of preference:

1. **Reinstall after the flash** (Phase 1). After the device is back, a typed `package_install` operation installs each third-party package from a source the administrator has registered: a feed URL, or a pinned file URL plus SHA-256. OpenManage downloads the file, checks the hash, uploads it, and installs it. The source list is an allowlist in the database, not a free-form field. Risk: the device briefly runs without the package (for a LuCI theme this just means falling back to the default theme), and the package must still match the new release.
2. **Bake into the image** (Phase 3, needs option B). Put the files in Image Builder's local `packages/` directory or add a custom feed whose signing key is trusted. The image then needs no network after flashing.
3. **uci-defaults first-boot script.** Useful for small settings (for example selecting the theme with `uci set luci.main.mediaurlbase`). Not a way to install packages, because the device may have no internet on first boot.

Per-package compatibility: before an upgrade, show which third-party packages are architecture-independent (typical for LuCI themes) and which are compiled, and warn when the source offers no build for the target release.

## Preserving and reapplying configuration

- Default path: `sysupgrade` with config kept (`-k`, the default), which restores `/etc/config` and files listed in `/etc/sysupgrade.conf` and package keep lists.
- Before flashing, take an OpenManage config snapshot (already exists) **and** a `sysupgrade -b` backup archive. The archive is stored encrypted with the existing AES-GCM key, never logged, size-capped, and kept for a bounded time.
- Why both: existing snapshots redact secrets and are read-only, so they cannot restore a device. They are used after the upgrade to show drift (what changed versus before), not to rewrite config. Restoring from the archive is the recovery path.
- After the flash and package reinstall, run the existing drift diff and show the result. Differences caused by release migrations are expected and are reviewed by the administrator, not silently overwritten.
- Large release jumps (for example 23.05 to 24.10) are blocked in Phase 1; only the same release family or the next one is allowed, with a warning.

## Safety

- **Preflight (read-only):** model/target match, upgrade path allowed, free RAM for staging the image in `/tmp`, enough flash, fresh config snapshot and backup archive exist, management path known (device reachable, pinned host key matches, a second path such as the LAN IP noted).
- **Download verification:** check the image's SHA-256 against the build service's response, and for official images against the release's `sha256sums` (and its signature, using OpenWrt's `usign` public keys, pinned in the app). Reject on any mismatch or unknown format. Then `sysupgrade -T` on the device validates the image for that board before flashing.
- **Confirmation:** explicit per-device confirmation showing the version change and the package list diff.
- **Staged rollout:** one canary device first, then batches with a small concurrency limit; stop the rollout automatically if a device fails verification. Never start fleet-wide upgrades until single-device upgrade and recovery are tested on real hardware (plan.md Section 5).
- **Verify after reboot:** wait for the device with a bounded timeout, require the pinned host key to match (the dropbear key is normally kept), check the new version, run the normal health check, check that expected packages are present, run the drift diff.
- **Recovery honesty:** if the device does not return, the job ends as `needs_attention` with plain guidance: wait for the timeout, power-cycle, failsafe mode, then serial/TFTP recovery. No claim of automatic rollback unless the hardware has a dual-partition setup that is actually verified.
- **Privacy:** option A sends model and package names to the build service. Show this once and make it an explicit opt-in setting.

## Fit with the operations engine

Everything runs as named typed operations with validated parameters, like `system.hostname` in config_changes.go. No generic shell and no free-form command fields.

New operations (names indicative):

| Operation | Class | Purpose |
| --- | --- | --- |
| `firmware_inventory` | read | Version, board, target, package manager, user-installed packages, free space |
| `firmware_backup` | read (secret-bearing output) | `sysupgrade -b` archive streamed, encrypted at once, never logged |
| `firmware_stage` | write | Upload the verified image to a fixed path under `/tmp` |
| `firmware_check` | read | `sysupgrade -T` on the staged image |
| `firmware_flash` | write, disruptive | Start `sysupgrade` detached; the connection will drop |
| `package_install` | write | Install a named package from a registered, hash-pinned source |

Write operations audit transactionally (a Section 2 rule). A job state machine in PostgreSQL (`planned`, `built`, `staged`, `flashing`, `verifying`, `done`, `needs_attention`) survives restarts, with no worker service: the existing server process drives it, like the monitoring loop. Migrations are additive.

## Phases

1. **Inventory and plan-only.** Capture manifests, show the package diff and what an upgrade would do. No writes.
2. **Single-device upgrade via ASU**, reinstalling third-party packages afterward, with backup, verification, and drift review. Test on real hardware and in the OpenWrt lab where possible (the lab is a container; flashing itself cannot be tested there).
3. **Staged rollout** with canary and concurrency limits.
4. **Optional local Image Builder** for baked-in third-party packages and offline builds.

## Open questions for Nishant

1. Is public ASU acceptable (model and package list sent to openwrt.org), or must builds be local?
2. How are third-party packages like `luci-theme-fluent` installed today (custom feed, or a downloaded `.ipk`/`.apk`)? That decides whether reinstall-after-flash is enough.
3. Which devices matter first, and do they have physical access if a flash fails? Any dual-partition devices?
4. Is `opkg` (24.10) enough, or do you want `apk` (25.x) support from the start?
5. Are large release jumps in scope, or only same-release and next-release upgrades?
6. For Phase 4, is a separate optional builder container acceptable?
