# Home setup on Proxmox: install, enroll devices, get ready for Wi-Fi sign-in

This walks through running OpenManage with Docker inside a Proxmox LXC container, enrolling an OpenWrt router and access points, and preparing for [Wi-Fi sign-in setup](radius-client-setup.md). Addresses below are examples: `192.168.1.10` is the container, `192.168.1.2` an AP. Every step is reversible; deleting the container removes everything.

## 1. Create the container

Access points send RADIUS over UDP 1812, and OpenManage accepts a request only if its source address is the AP's own LAN address. A container on the LAN bridge has its own address and receives packets unchanged, so no port forwarding tricks are needed.

In the Proxmox web UI, **Create CT**:

- **Template**: Debian 12 (download it under *local → CT Templates* if needed).
- **Unprivileged container**: yes. Under **Options → Features**, turn on **nesting** and **keyctl** (Docker needs both in an unprivileged container).
- **Resources**: 2 cores, 2 GiB memory, 16 GiB disk. The image build (frontend and Go) needs the memory; the running app stays well under 512 MiB.
- **Network**: bridge `vmbr0`, a static IPv4 address such as `192.168.1.10/24` with your router as gateway (or DHCP with a reservation). The APs are configured with this address.
- **Options**: Start at boot.

Or from the Proxmox shell (adjust the storage, template name and addresses):

```sh
pct create 120 local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst \
  --hostname openmanage --unprivileged 1 --features nesting=1,keyctl=1 \
  --cores 2 --memory 2048 --rootfs local-lvm:16 \
  --net0 name=eth0,bridge=vmbr0,ip=192.168.1.10/24,gw=192.168.1.1 \
  --onboot 1 --start 1
```

If the Proxmox firewall is enabled for the container, allow TCP 8080 and UDP 1812 from the LAN.

## 2. Install Docker and OpenManage

In the container's console (`pct enter 120`):

```sh
apt update && apt install -y curl git openssl openssh-client tcpdump
curl -fsSL https://get.docker.com | sh
docker run --rm hello-world
```

Then OpenManage:

```sh
git clone https://github.com/NishantJh/OpenManage.git /opt/openmanage && cd /opt/openmanage
cp .env.example .env
openssl rand -base64 32   # paste as OPENMANAGE_ENCRYPTION_KEY
openssl rand -base64 24   # POSTGRES_PASSWORD
openssl rand -base64 24   # OPENMANAGE_ADMIN_PASSWORD
```

Edit `.env` with those values. Uncomment and set `OPENMANAGE_IMAGE=ghcr.io/nishantjh/openmanage:latest` (the prebuilt image, so nothing is built on the container), and uncomment and set `OPENMANAGE_PORT=192.168.1.10:8080` so the dashboard is reachable from the LAN. That is plain HTTP: fine for a trusted home LAN, but read the Quick start note in the README before using it from guest or shared networks. Store the encryption key in your password manager: without it, the stored SSH keys and RADIUS secrets cannot be recovered ([Backup and recovery](backup-recovery.md)).

```sh
docker compose pull app
docker compose up -d
docker compose logs -f app   # wait for "OpenManage listening on :8080", then Ctrl+C
```

Sign in at `http://192.168.1.10:8080` as `admin` with the password from `.env`. Both services use `restart: unless-stopped`, so they come back when the container boots.

### Updating

Every merge to `main` publishes a prebuilt image to GitHub Container Registry, and `OPENMANAGE_IMAGE` in `.env` (set in step 2) points at it, so the container never builds anything. For each update:

```sh
cd /opt/openmanage
git pull
docker compose pull app
docker compose up -d
```

`git pull` brings in any `compose.yaml` or `.env.example` changes; the image itself comes from `pull`. Database changes apply on start. To go back to a known version, set `OPENMANAGE_IMAGE` to `ghcr.io/nishantjh/openmanage:sha-<commit>` and run `docker compose up -d`. If `pull` is denied, the package is private: open the repository's **Packages** on GitHub, choose `openmanage`, and set its visibility to public (the image holds no secrets; they come from `.env`). Only if you leave `OPENMANAGE_IMAGE` unset does Compose use a locally built image (`docker compose up -d --build`); that is not the recommended route.

## 3. Add each OpenWrt device

You need only the device's LAN IP address and its **root password** (the one you use to sign in to LuCI; set one first if the router has none). On **Devices → Add device**:

1. Enter the IP address and choose **Find device**. OpenManage reads the router's SSH host-key fingerprint and shows it. On a trusted home network you can accept it; if you want to be certain, compare it with the router's own value (`dropbearkey -y -f /etc/dropbear/dropbear_ed25519_host_key`, then `ssh-keygen -lf`). Every later connection must present exactly this key.
2. Enter the root password and choose **Add and connect**. OpenManage signs in once, installs a key it generates for this device, checks that key login works, then discards the password.

**What happens to the root password:** it is used for that one login only. It is not saved in the database, written to logs, or recorded in the audit trail, and the server clears its copy straight after the login. It stays valid on the router (OpenManage never changes it), so once the key works you can turn off SSH password login in LuCI (**System → Administration → SSH Access**). Use HTTPS if you open OpenManage from another machine, because the password travels from your browser to OpenManage. To remove OpenManage's access later, delete the line ending in `openmanage` from `/etc/dropbear/authorized_keys` on the router.

The wizard then runs a health check, which fills in the model and firmware.

### Advanced: bring your own key

The wizard's **Advanced / manual** link keeps the older path: add the device as inventory, then **Enroll** with a key you made. In the container create the key with `ssh-keygen -t ed25519 -N "" -C openmanage -f ~/.ssh/openmanage_ap1`, add the `.pub` contents under LuCI **System → Administration → SSH-Keys**, get the fingerprint from the router console as above, and paste the username, fingerprint and private key into **Enroll**. `host_key_mismatch` means the fingerprint is wrong or from another key type; `ssh_auth_failed` means the public key is not on the device.

## 4. Turn on RADIUS

1. Add `OPENMANAGE_RADIUS_LISTEN=:1812` to `.env`.
2. In `compose.yaml`, under the `app` service `ports:`, add the container's address: `- "192.168.1.10:1812:1812/udp"`.
3. `docker compose up -d`. The **Wi-Fi sign-in (RADIUS)** section now says RADIUS is listening and shows the server certificate.

Now continue with [Wi-Fi sign-in setup](radius-client-setup.md) from step 2 (Wi-Fi user and AP secret). The AP's `auth_server` is the container's address.

If the AP's requests do not arrive (no audit event when the phone tries to join), run `tcpdump -ni eth0 udp port 1812` in the container while the phone joins. Nothing at all means the AP points elsewhere or a firewall drops it; packets arriving while the dashboard's "Ignored packets" count rises means the source address or secret does not match the device's inventory entry.

## Backups

Take Proxmox backups of the container (vzdump) and follow [Backup and recovery](backup-recovery.md) for database dumps. A container backup includes `.env`, so store it as carefully as the encryption key. With device configuration backups enabled, a container backup or database dump plus the key is root-equivalent for every managed router and access point, so keep these files encrypted and off the host.
