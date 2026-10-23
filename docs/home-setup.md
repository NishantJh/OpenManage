# Home setup on Windows (WSL2): install, enroll devices, get ready for Wi-Fi sign-in

This walks through a fresh install on a Windows PC running Docker inside WSL2, enrolling an OpenWrt router and access points, and preparing for [Wi-Fi sign-in setup](radius-client-setup.md). On Linux, skip the WSL2 parts. Every step is reversible.

This is the WSL2 test setup. The planned production host is Docker inside a Proxmox LXC, and most steps apply there; skip the WSL2-only sections.

## 1. Networking choices (WSL2 only, do this first)

Access points send RADIUS over UDP 1812 to the OpenManage host, and OpenManage accepts a request only if its source address is the AP's own LAN address. Two things follow:

- **Use WSL2 mirrored networking.** In the default NAT mode the LAN cannot reach WSL2 on UDP at all (`netsh portproxy` forwards TCP only). Mirrored mode (Windows 11 22H2 or later, WSL 2.0 or later) gives WSL2 the PC's own LAN address. In `%UserProfile%\.wslconfig`:

  ```ini
  [wsl2]
  networkingMode=mirrored
  ```

  Then `wsl --shutdown` from PowerShell and reopen WSL. Allow inbound RADIUS through the Hyper-V firewall (PowerShell as administrator). Despite the name, this is the firewall Windows 11 applies to WSL2 by default (WSL 2.0.9 or later); it does not need the Hyper-V feature installed. The ID below is WSL's own:

  ```powershell
  New-NetFirewallHyperVRule -Name OpenManageRADIUS -DisplayName "OpenManage RADIUS" -Direction Inbound -VMCreatorId '{40E0AC32-46A5-438A-A0B2-2B479E8F2E90}' -Protocol UDP -LocalPorts 1812
  ```

  To undo: remove the `.wslconfig` line, `Remove-NetFirewallHyperVRule -Name OpenManageRADIUS`, and `wsl --shutdown`.

- **Use Docker Engine inside WSL2, not Docker Desktop.** Docker Desktop's port forwarding replaces the sender's address, so every RADIUS request would look like it came from Docker and be ignored. `docker info --format '{{.OperatingSystem}}'` prints "Docker Desktop" if that is what you have.

Give the PC a fixed address (a DHCP reservation on the router), because the APs are configured with it. WSL2 stops when no WSL window is open, and OpenManage with it; keep a WSL terminal open while testing.

## 2. Install OpenManage

In WSL2:

```sh
git clone https://github.com/NishantJh/OpenManage.git && cd OpenManage
cp .env.example .env
openssl rand -base64 32   # paste as OPENMANAGE_ENCRYPTION_KEY
openssl rand -base64 24   # POSTGRES_PASSWORD
openssl rand -base64 24   # OPENMANAGE_ADMIN_PASSWORD
```

Edit `.env` with those values. Keep `OPENMANAGE_PORT=127.0.0.1:8080`; with mirrored networking the Windows browser reaches it at `http://localhost:8080`. Store the encryption key in your password manager: without it, the stored SSH keys and RADIUS secrets cannot be recovered ([Backup and recovery](backup-recovery.md)).

```sh
docker compose up -d --build
docker compose logs -f app   # wait for the listening line, then Ctrl+C
```

Sign in at `http://localhost:8080` as `admin` with the password from `.env`.

## 3. Prepare each OpenWrt device

OpenManage signs in to each device over SSH with its own key and never sets the key up on the device itself. For each router or AP:

1. Make sure root has a password (LuCI asks on first login), so SSH does not allow a passwordless login.
2. In WSL2, create a management key with no passphrase (OpenManage needs it unencrypted):

   ```sh
   ssh-keygen -t ed25519 -N "" -C openmanage -f ~/.ssh/openmanage_ap1
   ```

3. Add the public key to the device: in LuCI, **System → Administration → SSH-Keys**, paste the contents of `~/.ssh/openmanage_ap1.pub` and save. (Or `ssh root@192.168.1.2 'cat >> /etc/dropbear/authorized_keys' < ~/.ssh/openmanage_ap1.pub`.)
4. Read the device's host-key fingerprint over a connection you trust (a wired SSH session on your own LAN, or the serial console). On the device, print the public host key:

   ```sh
   dropbearkey -y -f /etc/dropbear/dropbear_ed25519_host_key | grep ^ssh-ed25519
   ```

   Then in WSL2, turn that line into the fingerprint OpenManage expects:

   ```sh
   echo 'ssh-ed25519 AAAA... root@OpenWrt' | ssh-keygen -lf - | awk '{print $2}'
   ```

   The result starts with `SHA256:`.

One key per device is cleaner (losing one does not expose the others), but one shared key also works.

## 4. Add and enroll devices

1. **Add device**: a name, and the device's LAN IP address as a literal (for example `192.168.1.2`). For an AP this must be the address it sends RADIUS from, normally its `br-lan` address. Give each device a DHCP reservation or a static address.
2. **Enroll** on the device's row: username `root`, the fingerprint from step 3, and the contents of the private key `~/.ssh/openmanage_ap1` (the file without `.pub`).
3. Run a **health check**. It should show the model and firmware read from the device.

Error codes on enrollment are explained in the dialog; `host_key_mismatch` means the fingerprint is wrong or it came from another key type, and `ssh_auth_failed` means the public key is not on the device.

## 5. Turn on RADIUS

1. Add `OPENMANAGE_RADIUS_LISTEN=:1812` to `.env`.
2. In `compose.yaml`, under the `app` service `ports:`, add the PC's LAN address: `- "192.168.1.10:1812:1812/udp"`.
3. `docker compose up -d`. The **Wi-Fi sign-in (RADIUS)** section now says RADIUS is listening and shows the server certificate.

Now continue with [Wi-Fi sign-in setup](radius-client-setup.md) from step 2 (Wi-Fi user and AP secret). The AP's `auth_server` is the PC's LAN address.

If the AP's requests do not arrive (no audit event when the phone tries to join), check from WSL2 with `sudo tcpdump -ni any udp port 1812` while the phone joins. Nothing at all means the firewall rule above or mirrored mode is not in effect; packets arriving while the dashboard's "Ignored packets" count rises means the source address or secret does not match the device's inventory entry.
