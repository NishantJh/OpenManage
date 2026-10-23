# Wi-Fi sign-in with built-in RADIUS: setup and iPhone test

This guide turns on OpenManage's RADIUS server, points one OpenWrt access point at it, and joins an iPhone to a WPA2-Enterprise network with a per-person username and password. It is also the checklist for the first real-device test (plan.md Section 7). Everything here is reversible: removing the AP settings and unsetting `OPENMANAGE_RADIUS_LISTEN` returns to the previous state.

## What you need

Starting from scratch? [Home setup](home-setup.md) covers installing OpenManage in a Proxmox LXC, enrolling the AP, and turning on RADIUS.

- OpenManage running with `OPENMANAGE_ENCRYPTION_KEY` set, and the AP already added to the inventory with its LAN IP address (the same address the AP will send RADIUS requests from).
- An OpenWrt AP with a full `wpad` package. The `wpad-basic-*` builds may lack WPA-Enterprise; check with `opkg list-installed | grep wpad` and install `wpad-mbedtls` (or `wpad-openssl`) if needed.
- A test SSID you can change freely. Do not convert your everyday network until the test passes.
- An iPhone on iOS 16 or later.

## 1. Turn on RADIUS in OpenManage

1. Add to `.env` (use the OpenManage host's LAN address):

   ```
   OPENMANAGE_RADIUS_LISTEN=:1812
   ```

2. In `compose.yaml`, under the `app` service `ports:`, add a UDP mapping bound to the LAN address only:

   ```yaml
   - "192.168.1.10:1812:1812/udp"
   ```

3. `docker compose up -d`. The app log shows `RADIUS listening on udp :1812` and the CA fingerprint.
4. In the dashboard, the **Wi-Fi sign-in (RADIUS)** section now says RADIUS is listening and shows the **Server certificate** card. Note the CA fingerprint.

Never forward UDP 1812 from the internet.

## 2. Create a Wi-Fi user and the AP secret

1. **Add Wi-Fi user**, for example `nishant-iphone`, with a 12+ character password.
2. In **Access points with a secret**, press **+** on the AP's row and copy the secret from the dialog. It is shown only once; if lost, rotate it.

## 3. Configure the AP

On the AP (SSH), add a separate wireless interface for the test so your everyday network is untouched. `omtest` is a new section name; `radio0` is the radio to use (`uci show wireless | grep =wifi-device` lists them, and `radio1` is usually 5 GHz):

```sh
uci set wireless.omtest=wifi-iface
uci set wireless.omtest.device='radio0'
uci set wireless.omtest.mode='ap'
uci set wireless.omtest.network='lan'
uci set wireless.omtest.ssid='Home-Secure'
uci set wireless.omtest.encryption='wpa2'
uci set wireless.omtest.auth_server='192.168.1.10'   # OpenManage host
uci set wireless.omtest.auth_port='1812'
uci set wireless.omtest.auth_secret='<secret from step 2>'
uci commit wireless
wifi reload
```

To use an existing interface instead, find its section name with `uci show wireless | grep =wifi-iface` (for example `default_radio0`) and use it in place of `omtest`, skipping the first four lines.

To undo: `uci delete wireless.omtest; uci commit wireless; wifi reload`.

## 4. Install the profile on the iPhone

1. In the Server certificate card, type the SSID (`Home-Secure`) and press **Download setup profile**. Open the file on the iPhone (AirDrop it, or email it to yourself).
2. Settings → **Profile Downloaded** → Install. iOS says the profile is not signed; that is expected. The profile contains the OpenManage CA and the network settings, and no password.
3. Check the profile details show the CA named "OpenManage RADIUS CA ..." before installing.

## 5. Test checklist

Record pass or fail for each step. The dashboard's **Recent activity** shows `radius.auth_accept` / `radius.auth_reject` events with the username and reason.

| # | Step | Expected |
|---|------|----------|
| 1 | Join `Home-Secure`; enter the Wi-Fi username and password when asked | Connects without a certificate warning; Safari loads a page. Audit: `radius.auth_accept`, method `eap-ttls` |
| 2 | Forget the network, join again with a wrong password | Fails. Audit: `radius.auth_reject`, reason `bad_credentials` |
| 3 | Disable the Wi-Fi user in the dashboard; turn Wi-Fi off and on | Cannot join. Audit: reason `disabled`. Re-enable afterwards |
| 4 | Turn Wi-Fi off and on, then restart the phone | Rejoins on its own without asking |
| 5 | Leave it connected for 15 minutes and browse | Stays connected (the keys the AP received are correct) |
| 6 | Optional: remove the profile and join `Home-Secure` manually from the Wi-Fi list | iOS shows a certificate prompt for `radius.openmanage.home.arpa`. Do not trust it unless the fingerprint matches the dashboard. Manual join may pick an inner method other than PAP; if it fails with reason `inner_method_not_pap`, that is a known limitation and the profile is the supported path |

If step 1 fails, check in this order:

- **No audit event at all**: the request did not reach OpenManage or was dropped. Check the dashboard's "Ignored packets" count. If it rises, the AP's source address or secret does not match (RADIUS requests must come from the AP's inventory address).
- **`tls_failed`**: the phone refused the server. Check the profile's SSID and that it was installed.
- **`inner_method_not_pap`**: the phone was not using the profile.
- **Audit shows accept but the phone does not connect**: the key delivery to the AP failed. Capture `logread | grep hostapd` on the AP and share it.

## 6. Clean up

Remove the test interface from the AP (`uci delete wireless.omtest; uci commit wireless; wifi reload`), and in the dashboard remove the AP's RADIUS secret. To turn RADIUS off, unset `OPENMANAGE_RADIUS_LISTEN`, remove the port mapping and restart. The CA is kept, so reinstalled profiles keep working when RADIUS is turned on again.
