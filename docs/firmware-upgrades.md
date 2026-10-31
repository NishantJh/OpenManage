# Firmware build and upgrade: design

Status: in progress (started 2026-10-29). Phase 0 (backups) is built; Phase 1 has the read-only inventory (`firmware_inventory.go`) and upgrade plan (`firmware_plan.go`). It refines plan.md Section 5.

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

As built (`firmware_inventory`, `POST`/`GET /api/devices/{id}/firmware-inventory`; admins and operators capture, every role reads):
- The device's package database is read directly: the opkg status file, or the apk installed database and `/etc/apk/world`.
- "Installed by the owner" means: opkg packages flagged `user` that are not in the image, or apk world entries that are not in the image's world. "Removed defaults" are image packages that are no longer installed. Both need `/rom` (the squashfs image). When it is missing (x86 ext4 images, the lab container) the result is marked `rom_known: false`. On apk without `/rom`, every world entry is listed as installed by the owner.
- Feed-origin detection against the official index waits for the upgrade plan step, which is the first one that contacts the build service.
- Custom feed lines lose URL credentials and query strings before they are stored. Extroot means an enabled fstab mount targets `/overlay`.
- The newest 20 inventories per device are kept. A capture that differs only in free space and memory updates the newest row instead of adding one.

### Upgrade plan (as built)

Decided with Nishant 2026-10-29: every release the build server offers is listed (final releases, release candidates and snapshots), newest first, and the admin picks one, as in the ASU web client. apk (25.x) is supported from the start. The order agreed is: single-device upgrade with official packages first, then the package source registry.

- **Opt-in:** `firmware_settings` (one row) holds `build_server_enabled` (off by default) and `build_server_url` (default `https://sysupgrade.openwrt.org`). Only an administrator changes it; changes are audited. The URL must be https, or plain http to a private IP literal (a self-hosted server on the LAN). No credentials, query or fragment.
- **What is fetched**, read-only and cached for an hour: the build server's `/json/v1/overview.json` (branches, versions, renames, upstream URL), the upstream `profiles.json` for the release and target, and the build server's target and architecture package indexes (`/json/v1/<dir>/targets/<target>/index.json`, `/json/v1/<dir>/packages/<arch>-index.json`). Requests carry only release, target and architecture in the path. Bodies are capped at 16 MiB, requests time out after 20 s, and redirects to another host are refused.
- **Profile:** the board name with commas as underscores, else a profile that lists it under `supported_devices`, else `generic` (x86), as ASU and owut do.
- **Packages** (the owut "fs-user" form, additive to the release defaults): the owner's packages that the feeds still offer, after the branch's `package_changes` renames and removals up to the target revision, plus `-name` for each default the owner removed that is still a default. Owner packages the feeds lack are listed as "install again afterwards" and not requested.
- **Blockers:** extroot, no board name, no image for the board or target, and going from apk back to an opkg release. **Warnings:** same release, downgrade, snapshot, release candidate, new branch, opkg to apk, packages missing from the feeds, and no `/rom` to compare with.
- API: `GET`/`PUT /api/firmware/settings`, `GET /api/firmware/releases`, `GET /api/devices/{id}/firmware-plan?version=` (admins and operators). The plan uses the newest stored inventory.

### Comparing a device with the stock image (as built)

`GET /api/devices/{id}/firmware-stock-diff` (administrators and operators; needs the build server turned on) compares the device's newest inventory with the official image for the release it runs and its device profile. The stock list is the profile's default packages from upstream `profiles.json` (target defaults plus the profile's device packages, minus its `-name` entries), the same list ASU and the ImageBuilder start from.

- **Added:** packages the owner installed (`user_installed`) that are not on the stock list.
- **Removed:** stock-list packages the device does not have.
- Packages installed and later uninstalled appear in neither list, and library dependencies are not listed as removed, because the stock list names only what an image build is asked for. A library installed under a dated name matches its plain stock name.
- The Firmware page shows these two lists instead of "Installed by you" and "Removed from the image" when the build server is on, and tags added packages in the all-packages table. Without the build server it falls back to the device's own image copy (`/rom`), which can list packages that were installed and removed separately, and says so.

### Preparing an upgrade (as built)

Step 3, part 1. Approved by Nishant 2026-10-29: confirmation is typing the device name (no password), settings are always kept (the flash command can never carry `-n` or `-F`), only the same release family or the next one the build server lists is allowed (never a downgrade or a snapshot), and the first flash test is an OpenWrt x86-64 VM before his spare TP-Link Archer AX23.

Preparing never flashes and never touches clients; its only device write is one file under `/tmp`.

- **Job:** table `firmware_upgrades`, one open job per device (partial unique index). While a job is open, a restore or a configuration change on that device is refused.
- **Steps:** read the inventory again, check the upgrade path, build the plan, `POST /api/v1/build` on the build server and poll (15 minutes at most), download the `sysupgrade` image (squashfs preferred; refused if the choice is ambiguous; 64 MiB cap), compare its SHA-256 with the hash the build server reported, check free RAM (image size plus 16 MiB), take and pin a backup, upload the image on stdin to a fresh mode-700 directory `/tmp/.om-firmware`, compare its hash on the device, record the boot id, run `sysupgrade -T`.
- **Ready:** the job stores the image name, size and hash, the inventory digest and the boot id; the image itself is kept in memory only during the job. A ready job expires after 30 minutes: the staged file is removed and the backup unpinned if the job pinned it. Cancel does the same. A server restart ends preparing and ready jobs the same way.
- **What the hash proves:** images ASU builds to order are not in the release's signed `sha256sums`, so the hash shows the image is the one the build server says it built, and authenticity rests on HTTPS to the build server. Release signatures apply to stock images only.
- API (admins and operators): `POST /api/devices/{id}/firmware-upgrades` with `{"version"}`, `GET` the same path, `POST /api/devices/{id}/firmware-upgrades/{job}/cancel`. Audited: `device.firmware_upgrade_prepare`, `_ready`, `_stopped`.
- The flash and the post-reboot checks are described next; the screens for all of it are the next part of step 3.

### Flashing and verifying (as built)

- **Flash** (`POST /api/devices/{id}/firmware-upgrades/{job}/flash`) is admin only and needs `confirm_name` equal to the device name, and `acknowledge_warnings` when the plan has warnings. The server claims the device with a row lock, so a restore or config change cannot start at the same moment, and moves a still-ready, unexpired job to `flashing`.
- **Re-checks on the live device** before anything is sent: same boot id as when the image was staged, same package and release digest, the staged image still has its hash, and `sysupgrade -T` still accepts it. Any failure ends the job as `stopped` with "not flashed", removes the staged file and unpins the backup.
- **The one command** reads the expected hash from stdin, checks it on the device, then starts `sysupgrade image.bin` detached. It never carries `-n` or `-F`, so settings are always kept. `flash_sent_at` is recorded before the command is sent; after that the server only watches and never sends a second flash.
- **Verification** polls every 5 seconds for up to 10 minutes. A different boot id counts as a restart. The same boot id three minutes after the command means the device never restarted: the job needs attention and the staged file is removed. After a restart, in order: pinned host key and OpenManage's key (a mismatch stops everything and is never re-pinned), the release equals the target and the board name is unchanged, the normal health check, every package the plan asked for is installed (a missing one is a warning), and a configuration snapshot compared with the one before (differences are a warning for review and are never rewritten).
- **Outcomes:** `done`, `done_with_warnings`, `needs_attention`. The pinned backup stays pinned until a person acknowledges the result (`POST .../acknowledge`, admins and operators). A server restart while flashing or verifying resumes watching if the command may have been sent, and stops the job otherwise.
- **Tested** in the container lab for the claim, the re-checks and the "never restarted" outcome; the post-reboot checks need a real restart and are tested in the VM step before the first real device.

### Real restart test (OpenWrt VM)

`scripts/openwrt-vm-flash-test.sh` (GitHub workflow "OpenWrt VM flash", run it by hand) boots an official OpenWrt x86-64 image in QEMU, enrolls it, prepares an upgrade to the next release and flashes it. A small stand-in for the ASU build server (`scripts/openwrt-vm/asu.py`) hands out the official image of the next release, whose checksum is first checked against the published `sha256sums`. After the VM restarts, the script requires the job to end as `done` with every check `ok`, a new boot id, the new release, the staged file gone, and the pinned backup released only on acknowledge. It starts on 24.10.3 and flashes 24.10.4 by default; both are inputs. It does not test a real ASU build or package changes, and it is not a hardware test: the first flash on real hardware is still done by hand on a spare device.

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

### Package sources (as built, registry only)

Registering a package source installs nothing on any device yet; installing and reinstalling after a flash come in later PRs.

- **Endpoints:** `POST /api/package-sources` (administrators; a multipart upload with a `file` field, or JSON `{"url": "https://..."}`), `POST /api/package-sources/{id}/confirm` with `{"sha256": "..."}`, `GET /api/package-sources` (administrators and operators; operators see confirmed sources only) and `DELETE /api/package-sources/{id}` (administrators). File contents are never returned.
- **Formats:** `.ipk` (OpenWrt's gzip-compressed tar, or an ar archive) and `.apk` (apk-tools 3, the ADB format, which is raw-deflate compressed). The metadata is read with bounded reads and nothing is extracted or executed: name, version, architecture, description, dependencies and the full text of install scripts (`preinst`, `postinst`, `prerm`, `postrm` for `.ipk`; those plus `trigger`, `pre-upgrade` and `post-upgrade` for `.apk`). Hidden characters (control characters, invalid UTF-8, bidirectional overrides) are shown as visible escapes rather than dropped, so the review shows what would run; a script over 64 KiB is refused rather than shown in part. Package name, version and architecture must be well-formed. The readers were checked against real packages from the 24.10 and 25.12 feeds. Only `.ipk` control archives compressed with gzip are supported.
- **Downloads:** a link must be a GitHub release file (`https://github.com/<owner>/<repo>/releases/download/<tag>/<file>`); any other file is uploaded. https only, no credentials in the link, at most 5 redirects (each to github.com or githubusercontent.com), 32 MiB, 90 seconds. The address actually connected to must be public, so a link cannot reach this machine or the home network; no proxy is used. Only the link without its query string is kept (and audited).
- **Review and confirm:** a registered file is stored as *pending* for one hour. An administrator confirms it by sending the SHA-256 shown on the review screen, which must match; only then is the source usable. The same file cannot be registered twice. At most 100 sources and 512 MiB are stored.
- **Audit:** `package_source.create`, `package_source.confirm` and `package_source.delete`, each with name, version, hash and the script names.

### Installing a package source (as built)

`POST /api/devices/{id}/package-installs` (administrators only) installs a confirmed source on one device; `GET` lists the device's recent installs. It returns at once and the install runs in the background, one per device. It never runs on its own.

- **Asked for in full:** the request names a confirmed source, repeats the device name exactly, and acknowledges that the package's install scripts run as root on the device. A pending source is refused.
- **Checks before anything is sent:** the file format matches the device's package manager (`.ipk` for opkg, `.apk` for apk); the package architecture equals the device's (`all` and `noarch` always fit); free flash is at least twice the largest of the file size, the package's claimed installed size and, for an `.ipk`, the unpacked size measured from its data archive when it was registered (an `.apk`'s size is only the package's claim and is reported as a warning), plus 1 MB; free memory covers the file plus 16 MB; for `.ipk`, every dependency is already installed (`.apk` dependencies are left to apk); the same version is not already installed; and a package that came with the firmware or as a dependency is never replaced (a package the owner installed, such as an older theme, is). Any failed check ends the install with nothing sent.
- **On the device:** the stored copy goes in on stdin to a fresh mode-700 directory (`/tmp/.om-pkg`), its SHA-256 is checked there, then one fixed command installs that one file: `opkg install <file>` or `apk add --no-network --allow-untrusted <file>`. apk applies `--allow-untrusted` to the whole transaction, so `--no-network` keeps it to that one hash-checked file: nothing is fetched from a feed while signatures are off, and an `.apk` whose dependencies are not installed fails with nothing changed. The directory is removed afterwards, whatever happened.
- **Verified from the device:** the package list is read again and must show the package at the expected version. The command's exit status alone is not trusted.
- **Exclusion:** no install while a firmware upgrade is open, a restore is running or a configuration change is applying, and none of those start while an install runs. A server restart ends a running install as failed with a note to check the device.
- **Audit:** `device.package_install` records the source, version and hash. The package manager's output is never stored: an install script runs as root and could print a secret, and operators can read install records.
- **Tested** on a real OpenWrt VM in the opt-in "OpenWrt VM flash test" workflow, after the flash: register, review, confirm, install, check the files and the post-install script on the VM, and a second install of the same version refused.

### Removing a package now (Step 5c)

`POST /api/devices/{id}/package-removals` starts a typed, administrator-only removal job; `GET` lists recent removal attempts. This is an explicit apply-now action, separate from editing a desired package set.

- The request must contain the exact package name twice (selection and confirmation), the exact device name, `apply_now: true`, and acknowledgement that package removal scripts may run as root.
- Only a syntactically valid package name that is currently installed and marked user-installed by the device inventory may be removed. Firmware defaults, dependency-installed packages, protected management/network/security packages, kernel packages and kernel modules are refused.
- The device's package manager selects one fixed command: `opkg remove <name>` or `apk del --no-network <name>`. The package name is validated before it is included; callers cannot provide command text or arguments. Package-manager output is discarded and never stored.
- OpenManage reads the package inventory before and after the operation. The job is successful only when the named package is absent afterward. The command's exit status alone is not enough.
- Only one package operation (install or remove) may run on a device. Firmware upgrades, restores and configuration changes remain mutually exclusive with package operations. Interrupted jobs are marked failed on server restart with a note to verify the device.
- `device.package_remove` records the device and package name. No package is removed automatically by firmware planning, flashing, desired-set editing or reinstall reporting.

### Reinstalling after a flash (as built)

The packages the official feeds do not offer (the plan's "unavailable" list, typically third-party themes) are left out of the image on purpose. After the device restarts, the upgrade's checks add **Third-party packages**: for each of those packages the device does not have, the newest confirmed package source that fits the device (same name; `.ipk` for opkg or `.apk` for apk; architecture `all`/`noarch` or equal to the device's) is named, or the reason none fits (no source, wrong format after an opkg to apk switch, wrong architecture). Any missing package makes the upgrade end "done with warnings".

**Nothing is reinstalled by the flash.** `GET /api/devices/{id}/package-reinstalls` lists the same candidates from the newest finished upgrade and the newest inventory; an administrator then installs each one with the install request above, typing the device name. Tested in the OpenWrt VM workflow: a package is installed from the app, the VM is flashed, the upgrade reports it missing with its source, the flash did not bring it back, and an administrator installs it again.

### Software sources screen and update check (as built)

- **Screen:** the Firmware page has a Software sources section. Administrators add a package (upload or GitHub release link), read its description, dependencies and install scripts, and confirm by repeating the SHA-256. Operators see confirmed sources. Per device it shows installs, results, and the packages that need reinstalling after a flash, each with an Install button that opens the same typed-device-name confirmation.
- **Check for update:** `POST /api/package-sources/{id}/check-update` (administrators; confirmed sources registered from a GitHub release link). It asks GitHub for the latest release tag (github.com only, same address checks as downloads), works out the matching file link by replacing the old tag in the file name, and returns `up_to_date` or `newer` with the link. It downloads and registers nothing; the administrator presses "Download for review" and the new file is a new pending source with the normal review and confirmation. It runs only when the button is pressed; there is no background check.

### Desired package sets (Steps 5a–5b)

PR #132 adds a per-device desired package set with immutable, numbered revisions. Each revision records official packages to add, firmware defaults to remove, and confirmed package sources either pinned to a registered source id/version or following the latest confirmed source registered for that package name. The actor and timestamp are retained, and restoring an earlier revision creates a new revision rather than mutating history.

- API: `GET` and `PUT /api/devices/{id}/package-set`; `POST /api/devices/{id}/package-set/revisions/{revision}/restore`. Reads are available to device readers; writes and restores are administrator-only.
- The migration is additive and re-runnable. Package source rows referenced by a pinned revision cannot be deleted; the last confirmed source for a name referenced in latest mode cannot be deleted.
- Step 5a stores desired state only. Step 5b snapshots the current revision into each firmware plan: official additions are included only when present in the target release feeds, default removals are included only when the package is a target-release stock default, and missing official additions are called out explicitly. The plan records the revision number.
- Scope decision: no device-group model exists, so inheritance is deferred. No automatic post-flash reinstall is introduced; the existing report-and-manual-install behaviour remains.

- Custom package sources are resolved when the firmware plan is prepared. Both `pinned` and `latest` tracking record the selected source id and version in the plan; `latest` means the latest confirmed source at plan time, not an unreviewed source swap during flashing. After the flash, the reinstall report uses that exact source and reports compatibility or availability problems. It never installs automatically; an administrator still starts the typed install operation.

### The package set changes over time

The desired package set is per device; there is no device-group model or inheritance yet. It contains official packages to add, firmware defaults to remove, and custom package sources. Every edit creates an immutable, audited revision. Restoring history creates another revision rather than rewriting an earlier one.

Editing the set changes desired metadata only. The selected revision is incorporated into a newly prepared firmware plan. It does not change a running device, an existing plan, or start a package operation. Official additions are included only when present in the target release feeds; default removals apply only to stock defaults in the target release. Defaults needed to reach and manage the device (kernel and kmod-* modules, base-files, busybox, libc, procd, ubus, uci, rpcd, netifd, dropbear, openssh, apk, opkg) can never be removed: saving such a set is refused, and an older revision that names one makes the plan show a blocker. Swaps of wpad or dnsmasq variants stay possible.

Custom sources must be registered and confirmed first. A set can pin an exact source id/version or follow the latest confirmed source for that package name; the firmware plan snapshots the resolved source id/version. A newer source is not silently substituted into an already prepared plan. Unused sources can be deleted only when no pinned or latest-tracking revision still depends on them.

Package install and package removal are separate, explicitly administrator-started operations. They are not an implicit “apply now” action on desired-set edits, and they do not automatically rewrite the desired set. Automatic post-flash reinstall is not supported: the post-flash report identifies candidate sources, and an administrator starts the typed install flow manually.

Drift reporting against the desired package set is deferred. The current UI provides the desired-set editor and revision history, while the firmware plan is the point at which desired state is compared with the target release.

### What an upgrade keeps and adds

One upgrade does three things, in this order:
1. **Keeps what is installed.** The build request carries every package the device has, plus removed defaults (see "Common cases").
2. **Keeps configuration and other changes.** Config and extra files are restored from the sysupgrade backup (see "Preserving and reapplying configuration").
3. **Adds the custom packages** from the device's desired set.

**Auto-reinstall option** (per device or group, on by default once a set has custom packages): after each firmware update, OpenManage reinstalls the custom packages from their stored files without asking. If switched off, the custom packages are listed as "pending" after the flash and an admin installs them with one action. In both cases the set stays as it was, so the next upgrade does the same. Between upgrades the admin can add more custom packages or remove some by editing the set (applied on the next flash, or "apply now").

### Using sources during an upgrade

1. **Reinstall after the flash** (Phase 2, default). Once the device is back, `package_install` reinstalls each third-party package from its source. Risk: the device briefly runs without the package (for a LuCI theme it falls back to the default theme), and the package must still suit the new release; a LuCI theme built for another LuCI version can fail to load.
2. **Baked into the image: not supported for file sources.** ASU can only add packages through a feed allow list, not single files, so file and upload sources are always reinstalled after the flash. Feed support (and baking) is a possible later addition.
3. **uci-defaults first-boot script.** For small settings only (for example selecting the theme with `uci set luci.main.mediaurlbase`). Not a way to install packages, because the device may have no internet on first boot. Normally unnecessary because the config backup already restores settings.

Compatibility warnings before an upgrade: list third-party packages that are architecture-independent (typical for LuCI themes) versus compiled, and warn when a source offers nothing for the target release (feed index has no match, or the file's declared dependencies do not fit).

Out of scope: building packages from source, and a package store or search across the internet.

## Common cases

Checked with Nishant 2026-10-23. Sources for the claims below are forum posts, not official docs, so confirm each in the lab.

1. **Swapping default packages** (for example `wpad-basic-*` to `wpad-openssl`, `dnsmasq` to `dnsmasq-full`). Replicate what the ASU clients do: read the installed package list from the device, add back any default packages the user removed, and send that to the server, which uses it nearly verbatim (per an OpenWrt developer on the forum). So the manifest records user-installed packages **and removed defaults**, and the build request carries both. The ASU API's diff-style package list and its `defaults` (uci-defaults) field are to be confirmed against the API docs.
2. **Third-party LuCI apps and themes:** the package source registry above.
3. **Proxy and VPN suites** (passwall, openclash and similar). Image Builder only assembles prebuilt packages, it does not compile anything, and ASU builds only from official feeds. So such a suite is supported only if the admin has a release file **built for the target release and architecture**, and its dependencies are in the official feeds (preflight checks both). Otherwise the upgrade plan shows it as "no build for the target release" and the admin decides. These suites are commonly rebuilt per release, so expect this to need attention at every release upgrade.
4. **Out-of-tree kernel modules and drivers.** Not supported. A kernel module must match the exact kernel build it loads into, the official Image Builder keeps the official kernel, and a module built for another build will not load; forced installs have reportedly caused kernel panics. Preflight blocks any package that depends on an exact kernel version and is not from the official feeds, with a plain message.
5. **Held package versions, custom files and scripts** (`rc.local`, hotplug scripts, certificates, `authorized_keys`): kept through the sysupgrade keep list (`/etc/sysupgrade.conf`). The upgrade plan shows that list so the admin can check it before flashing.
6. **Packages on a USB drive (extroot):** out of scope for now. Devices using extroot are detected and the upgrade is refused with an explanation.

## Preserving and reapplying configuration

- Default path: `sysupgrade` with config kept (`-k`, the default), which restores `/etc/config` and files listed in `/etc/sysupgrade.conf` and package keep lists.
- Before flashing, take an OpenManage config snapshot (already exists) **and** a `sysupgrade -b` backup archive, handled as described in "Device config backup (Phase 0)" below. The pre-flash archive is pinned until the upgrade is done and acknowledged.
- Why both: existing snapshots redact secrets and are read-only, so they cannot restore a device. They are used after the upgrade to show drift (what changed versus before), not to rewrite config. Restoring from the archive is the recovery path.
- After the flash and package reinstall, run the existing drift diff and show the result. Differences caused by release migrations are expected and are reviewed by the administrator, not silently overwritten.
- Large release jumps (for example 23.05 to 24.10) are blocked in Phase 1; only the same release family or the next one is allowed, with a warning.

## Device config backup (Phase 0)

A `sysupgrade -b` archive is root-equivalent for the router. As data it holds Wi-Fi PSKs, RADIUS shared secrets, the dropbear host private key, TLS and VPN private keys, and `/etc/shadow` (an offline-crackable root hash). As code, restoring it writes `rc.local`, hotplug and `uci-defaults` scripts, `authorized_keys` and `/etc/shadow`, then reboots. The rules below follow from that. Security review: [#72 comment](https://github.com/NishantJh/OpenManage/pull/72#issuecomment-6416376878).

**Who can do what**

| Action | Roles | Audit event (details) |
| --- | --- | --- |
| Create (manual) | admin, operator | `device.backup_create` |
| Scheduled run | system | `device.backup_create` with `scheduled: true` |
| List (metadata only) | any role that can see the device | none |
| Download | admin, after password re-entry | `device.backup_download` |
| Restore | admin, after re-entering their password and typing the device name | `device.backup_restore`, written in the same transaction that starts the job |
| Delete | admin | `device.backup_delete` |

Audit details carry only backup id, device id, size, keyed digest, actor and scheduled flag. Never file names from the archive and never content. Readonly users never see backup content.

**Capture**

- Fixed command, no parameters from the request. Prefer `sysupgrade -b -` to stdout (verify on 23.05 and 24.10 in the lab). Otherwise write to a fixed `/tmp` path under `umask 077` and remove it in the same command on exit. Check free RAM first, because `/tmp` is RAM.
- Output cap 4 MiB (typical archives are tens of KiB) and a fixed timeout. Stderr is dropped and replaced by a fixed message, since it can echo file names.
- Recorded with each backup: device id, board name, target, OpenWrt release, a MAC or board serial, size, a keyed digest of the plaintext (the `configDigest` pattern, not a plain hash), and the dropbear host-key fingerprint the archive contains.
- One backup or restore job per device at a time.

**Encryption**

- AES-256-GCM with the existing key and AAD `openmanage:firmware-backup:v1:<device_id>:<backup_id>`. The backup id is generated before sealing. A row moved to another device or id fails to decrypt.
- A key-version column, so the encryption-key rotation work (plan.md Section 4) can find and re-encrypt these rows.
- Plaintext lives only in memory. It is never written to a server file, logged, or put in an error message.

**The server never extracts the archive.** It is opaque bytes. Where OpenManage reads it (file list, restore preflight), it parses in memory with `archive/tar` behind `gzip`, with a decompressed-size cap, an entry-count cap and a per-entry size cap. It rejects absolute paths, `..`, symlinks, hardlinks and device nodes, and writes nothing to disk. Device output is untrusted: a compromised router can send anything. The keep list (`/etc/sysupgrade.conf`) is set by the device. OpenManage shows it read-only and never edits it in Phase 0.

**Download**

- Headers `Content-Disposition: attachment`, `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`. Streamed from memory.
- A warning when the session is on plain HTTP: the archive would cross the network unencrypted.
- The archive can also be downloaded encrypted with a passphrase the admin types (age, scrypt), so the downloaded file is not plaintext root material. Plain download is still available for use with LuCI.

**Restore** (`firmware_restore`, a write and disruptive operation)

- Only archives OpenManage captured. No uploaded archives in Phase 0; that needs its own review.
- The only parameter is a backup id. Commands are fixed strings: upload to a fixed `/tmp` path with mode 600, check `sha256sum` against the stored value, `sysupgrade -r`, remove the file, reboot.
- Same device id only. Refused if the live board name differs from the recorded one. A release mismatch needs a second confirmation, because `sysupgrade -r` does not migrate config across releases.
- Before upload: decrypt (GCM verifies integrity) and check the keyed digest.
- Lockout preflight: refuse with a plain message if the archive's `authorized_keys` lacks the current OpenManage public key, or its dropbear host key does not match the pinned fingerprint. The confirmation notes that the archive restores the root password as it was then.
- After reboot: the normal verify step (pinned host key, health check, drift diff).

**Retention and deletion**

- Keep the last 5 per device. An archive whose keyed digest equals the latest one is not stored again; the latest one's last-verified time is refreshed instead.
- The pre-flash backup is pinned (excluded from pruning) until the upgrade job is `done` and the admin acknowledges.
- Schedule: off by default, at most daily, run from the existing monitoring loop with its concurrency limit.
- Removing a device from inventory deletes its backups; the UI says so first.
- Honesty notes in the UI and docs: old backups keep old secrets, so after rotating a leaked Wi-Fi or root password, older backups still hold the old one and restoring brings it back (delete them after a rotation). Deleting a row does not remove it from earlier database dumps or Proxmox backups.

**Database and host backups.** With router archives in PostgreSQL, a `pg_dump` plus the key, or a Proxmox backup of the container (which includes `.env`), is root-equivalent for every managed router. [Backup and recovery](backup-recovery.md) and [home setup](home-setup.md) must say these files are stored encrypted and off-host.

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
| `firmware_restore` | write, disruptive, admin-only | Restore an OpenManage-captured archive to the same device; parameter is the backup id only |
| `firmware_stage` | write | Upload the verified image to a fixed path under `/tmp` |
| `firmware_check` | read | `sysupgrade -T` on the staged image |
| `firmware_flash` | write, disruptive | Start `sysupgrade` detached; the connection will drop |
| `package_install` | write | Install a package from a registered source (stored release file or upload, allow-untrusted); parameters are source id only |
| `package_remove` | write | Remove a named package that the desired set marks for removal; package name validated against the device manifest |

Write operations audit transactionally (a Section 2 rule). A job state machine in PostgreSQL (`planned`, `built`, `staged`, `flashing`, `verifying`, `done`, `needs_attention`) survives restarts, with no worker service: the existing server process drives it, like the monitoring loop. Migrations are additive.

## Phases

0. **Device config backup** (also a standalone feature): `firmware_backup`, encrypted storage, download, `firmware_restore`, retention, per "Device config backup (Phase 0)". Needed before any flash.
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


### Desired package set UI (Step 5d)

The Firmware page exposes the per-device desired package set. Administrators can edit official package additions, firmware-default removals, and custom package-source references. Custom sources must already be confirmed in Software sources; each entry can pin an exact registered source id/version or follow the latest confirmed source for that package name. The UI displays the current revision and the latest 50 immutable revisions. Restoring a revision creates a new revision based on the selected historical revision; it does not rewrite history.

These controls change desired metadata only. Saving or restoring does not contact a device, install or remove packages, or alter an existing firmware plan. The selected revision is incorporated when a new firmware plan is prepared (Step 5b). Custom packages remain manual post-flash installs; no automatic reinstall is enabled. Operators and readonly users can inspect revision history; only administrators can edit or restore. Package-source selection is available to administrators and operators, while readonly users see recorded source references without registry details.



## Step 6 — staged firmware rollout (planned; no fleet execution enabled)

Step 6 builds campaign coordination around the existing single-device firmware job. It does not replace that job or weaken its checks.

### 6a. Campaign drafts and immutable membership

An administrator selects the target release, exactly one canary device, and ordered batch membership. Draft creation snapshots each device's identity, latest stored inventory id/digest, and desired-package-set revision. It is metadata-only: no device contact, build-server request, package operation, or flash. A draft is not executable.

### 6b. Server-computed plan preview

The server computes and persists each member's plan from the draft's stored inventory and desired-package-set revision, then records a digest of the complete plan. It rejects missing/stale inventory, changed device identity or desired-set revision, unsupported release paths, and any plan blocker. A preview is immutable; a changed device, inventory, desired set, or target release requires a new draft.

### 6c. Physical-hardware qualification

Before a campaign may flash, an administrator must record a successful single-device upgrade and recovery drill on supported physical hardware. Record board/model, source and target release, date, checks, recovery path exercised, and outcome. The qualification must match the campaign's supported hardware. VM and container tests remain useful CI evidence but never satisfy this gate.

### 6d. Canary and batches

The canary is the only device eligible for the first flash. Its existing firmware job must reach `done`, all verification checks must be successful, and the result must be acknowledged before the next batch is released. Warnings are not a passing gate. Subsequent batches are released in order, with a small concurrency limit (initial maximum two). Every device still requires an individual administrator flash confirmation and warning acknowledgement through the existing endpoint.

Any `needs_attention`, `done_with_warnings`, timeout, unreachable device, stale plan, failed verification, audit failure, or ambiguous flash state pauses progression. No automatic retry is permitted after the flash command may have been sent. Recovery is operator-led; single-partition devices may need failsafe, reset, serial, or TFTP recovery.

### 6e. UI, audit and verification

The UI reports campaign, canary, batch, and per-device states and the exact reason a campaign is paused. Pause/cancel operations are audited and cannot imply that an already-sent flash was cancelled. Campaign state and device membership survive app restarts. Tests cover duplicate requests, concurrent device confirmations, stale inventory/package-set snapshots, partial failure, app restart during each state, audit failures, and enforcement of the physical-hardware gate.

### Explicit non-goals

No unattended fleet-wide flash, no single campaign action that silently approves all devices, no forced sysupgrade flags, no downgrade/snapshot/release skipping, no automatic package install/removal or reinstall, no retry after an ambiguous flash, and no claim of automatic rollback on single-partition hardware. The rollout controller must reuse the existing single-device prepare/flash/verify/acknowledge flow.
