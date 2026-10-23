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

### Online build services (checked 2026-10-23)

| Service | Third-party packages (e.g. `luci-theme-fluent`) | Trust and cost |
| --- | --- | --- |
| **Official ASU**, `sysupgrade.openwrt.org` (API docs at `/docs/`) | No. It uses the official ImageBuilder and official feeds. | Run by the OpenWrt project. Free. Sees the device model and package list. I could not confirm from the README whether it signs images, so rely on its returned SHA-256 and `sysupgrade -T`. |
| **Self-hosted ASU** ([openwrt/asu](https://github.com/openwrt/asu)) | Possibly. Forum reports describe an extra-feed allow list (`repository_allow_list`) and a signed index, but the feed must follow the official layout, and apk-format extra feeds were reported as not fully supported yet ([forum](https://forum.openwrt.org/t/how-to-add-self-compiled-packages-to-my-own-asu/247855)). Unverified; test before relying on it. | You control it, nothing leaves your network. Needs Podman and Redis (an extra service, against our rule), about 2 GB RAM and 50 GB disk. |
| **Image Builder in GitHub Actions** (a workflow you own) | Yes: add `.ipk`/`.apk` files to the builder's local packages directory or a custom feed. | You control the workflow; GitHub sees the build. Free tier minutes apply. Community templates mostly build from source, which is slower than Image Builder. OpenManage would fetch the finished artifact and verify its hash. |
| **Unofficial ASU servers** | Unknown | The ASU README says unofficial servers may run a modified ImageBuilder. I found no third-party service I would trust here; don't use one for firmware that runs on your router. |

So no online service builds third-party packages for you without you running something yourself. For fluent, the realistic options are reinstall after flash (Phase 1) or your own Image Builder run (local container or GitHub Actions).

Decision (Nishant, 2026-10-23): **public ASU is the default** (official packages, third-party ones reinstalled after the flash). **Self-hosted ASU is an optional mode** for users who want to run their own server, kept entirely outside the OpenManage stack so the core still has no extra services. A local Image Builder (B) is not planned unless the self-hosted mode proves insufficient. OpenManage never gets access to the Docker or Podman socket.

### Optional self-hosted ASU mode

- **Setting:** an administrator-set "build server URL" (default `https://sysupgrade.openwrt.org`). OpenManage only speaks the ASU HTTP API to it, so the same code serves both modes. The URL must be `https` or an explicitly allowed private address, and is validated like other inventory addresses (bounded timeouts, no redirects to other hosts).
- **One-command setup:** a script in the repo (for example `scripts/asu-server-setup.sh`) the user runs on a Linux machine, such as a separate VM or LXC, never inside the OpenManage container. Spec:
  - Checks: Linux, root or sudo, enough RAM and disk (ASU documents about 2 GB RAM and 50 GB disk as the minimum), and a container runtime. Installs Podman and podman-compose if missing, on Debian (and Ubuntu) only; it refuses other distributions.
  - Fetches a **pinned** ASU release (tag and checksum recorded in the script), writes the compose and config files, creates the Podman socket link and isolated build network that ASU requires, and starts the stack (server, Redis, worker).
  - Optionally takes extra feeds: `--feed URL --feed-key KEY_FILE` adds the feed to the server's allow list, so third-party packages can be built in. This is the part to test first: forum reports say extra-feed support exists but apk-format feeds are incomplete.
  - Idempotent (safe to re-run), prints the final URL, and offers `--uninstall`. Binds to a private interface by default and prints a warning before exposing it, because ASU has no authentication of its own (to check); a reverse proxy with TLS is recommended.
  - Never prints or stores secrets; any signing key it generates for a custom feed stays on that machine.
- **Trust:** the self-hosted server builds the images, so OpenManage's trust in an image equals trust in that machine. The same hash and `sysupgrade -T` checks apply. Nothing about the devices leaves the user's network.
- **Out of scope here:** the script itself (spec only for now), and managing the ASU server from OpenManage.

Profile mapping: ASU needs release, target, profile and package list. The profile comes from `board_name` (commas replaced with underscores). To be confirmed in the lab for each supported model.

## Third-party packages

Goal: adding an external package (for example the `luci-theme-fluent` theme) works the way OpenWrt users already do it, and is then remembered so every future upgrade restores it.

### Package sources

Decision (Nishant, 2026-10-23): custom packages come from a **release file** (a direct `.ipk`/`.apk` URL, typically a GitHub release asset, which is how many LuCI themes ship) or an **upload**, and are installed **allow-untrusted**, meaning the package manager's signature check is skipped for that file. Custom feeds are not part of the first version.

OpenManage keeps a **package source** registry (admin-managed, audited). A package is only ever installed from a registered source, never from a free-form URL typed into an operation.

**Registering a source:**
- Upload a file, or give a URL. For a URL, OpenManage downloads it once (https only, size capped, bounded timeout) and stores its own copy in PostgreSQL with the SHA-256 it computed. Later installs use the stored copy, so an upgrade does not depend on the upstream link still being alive or unchanged.
- OpenManage reads the file's control data and shows the admin: name, version, architecture, dependencies, and whether it carries install scripts (`postinst`/`prerm`, which run as root on the router). The admin confirms before the source is saved. A new version is a new, separately confirmed source.

**Installing** ("Install package" on a device or group): a typed `package_install` operation takes only a source id and the device. Preflight checks: architecture is `all` or matches the device; package format matches the manager (`.ipk` for opkg, `.apk` for apk); free flash; dependencies are satisfiable from the official feeds already configured on the device. OpenManage uploads the stored file to a fixed path under `/tmp`, checks its hash on the device, installs it, then removes it.

**What allow-untrusted means here:** with `apk` it is the `--allow-untrusted` flag; installing a local `.ipk` with `opkg` is believed not to be signature-checked (to confirm in the lab). Integrity then rests only on the stored hash and on the admin having reviewed the file, and the package runs as root. To limit this: the flag is used for that single stored file only and never set as a global option; the audit trail records source, hash, device and who confirmed it; and the UI says plainly that the package is unsigned and trusted by the admin's choice.

**Packages already on the device** (installed by hand before OpenManage): the inventory marks them "third-party, source unknown". The admin resolves each one by attaching a source (release file or upload). This is a one-time step and cannot be automated, because the package manager does not keep the original file. On `opkg`, a package from a custom feed the user configured by hand also shows as source unknown; the admin attaches the same kind of file source.

### Using sources during an upgrade

1. **Reinstall after the flash** (Phase 2, default). Once the device is back, `package_install` reinstalls each third-party package from its source. Risk: the device briefly runs without the package (for a LuCI theme it falls back to the default theme), and the package must still suit the new release; a LuCI theme built for another LuCI version can fail to load.
2. **Baked into the image: not supported for file sources.** ASU can only add packages through a feed allow list, not single files, so file and upload sources are always reinstalled after the flash. Feed support (and baking) is a possible later addition.
3. **uci-defaults first-boot script.** For small settings only (for example selecting the theme with `uci set luci.main.mediaurlbase`). Not a way to install packages, because the device may have no internet on first boot. Normally unnecessary because the config backup already restores settings.

Compatibility warnings before an upgrade: list third-party packages that are architecture-independent (typical for LuCI themes) versus compiled, and warn when a source offers nothing for the target release (feed index has no match, or the file's declared dependencies do not fit).

Out of scope: building packages from source, and a package store or search across the internet.

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
| `package_install` | write | Install a package from a registered source (stored release file or upload, allow-untrusted); parameters are source id only |

Write operations audit transactionally (a Section 2 rule). A job state machine in PostgreSQL (`planned`, `built`, `staged`, `flashing`, `verifying`, `done`, `needs_attention`) survives restarts, with no worker service: the existing server process drives it, like the monitoring loop. Migrations are additive.

## Phases

1. **Inventory and plan-only.** Capture manifests, show the package diff and what an upgrade would do. No writes.
2. **Single-device upgrade via ASU**, reinstalling third-party packages afterward, with backup, verification, and drift review. Test on real hardware and in the OpenWrt lab where possible (the lab is a container; flashing itself cannot be tested there).
3. **Staged rollout** with canary and concurrency limits.
4. **Optional self-hosted ASU mode:** the build-server URL setting and the setup script, with optional extra feeds (not needed for release-file or upload packages, which are reinstalled after the flash).

## Open questions for Nishant

1. (Decided) Public ASU by default, self-hosted ASU optional. Is the privacy note shown at first use enough?
2. Which third-party packages do you use today besides `luci-theme-fluent`? Any with install scripts or compiled code?
3. Which devices matter first, and do they have physical access if a flash fails? Any dual-partition devices?
4. Is `opkg` (24.10) enough, or do you want `apk` (25.x) support from the start?
5. Are large release jumps in scope, or only same-release and next-release upgrades?
6. (Decided) The setup script targets Debian (and Ubuntu) only at first.
7. (Decided) Release file or upload, installed allow-untrusted; feeds later if ever.
