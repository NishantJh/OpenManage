# Wi-Fi sign-in with built-in RADIUS: setup and iPhone test

This guide turns on OpenManage's RADIUS server, switches an OpenWrt access point's existing Wi-Fi networks to it, and joins an iPhone to WPA2-Enterprise with a per-person username and password. It is also the checklist for the first real-device test (plan.md Section 7). Everything here is reversible: removing the AP settings and unsetting `OPENMANAGE_RADIUS_LISTEN` returns to the previous state.

## What you need

Starting from scratch? [Home setup](home-setup.md) covers installing OpenManage in a Proxmox LXC, adding the AP, and turning on RADIUS.

- OpenManage running with `OPENMANAGE_ENCRYPTION_KEY` set, and the AP already added as a device, with its LAN IP address (the same address the AP will send RADIUS requests from).
- An OpenWrt AP with a full `wpad` package. The `wpad-basic-*` builds may lack WPA-Enterprise; check with `opkg list-installed | grep wpad` and install `wpad-mbedtls` (or `wpad-openssl`) if needed.
- Know what changes: the AP's existing networks keep their names and radios but switch to per-person sign-in. Every device on them disconnects; devices that only know the old Wi-Fi password, such as printers or smart plugs, cannot rejoin. To try it on one network first, name only that network when you get the secret (step 2).
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
2. Get the AP's secret. Tick **Also let this device check Wi-Fi sign-ins (RADIUS)** when you add the AP (Add device wizard, **Advanced / manual** form, or the key icon on a device without a key). For an AP that is already set up, press **+** (Create secret) on its row on the Configuration page. The secret is shown only once; if lost, rotate it.

The screen that shows the secret also shows every command in step 3, with the secret, port and OpenManage address filled in. The first time, type the OpenManage host's LAN IP in **OpenManage LAN address**; it is saved and filled in for every device after that. Leave **Wi-Fi networks to change** empty to change every network on the AP, or list network names (comma separated) to change only those.

## 3. Configure the AP

**Every device on the changed networks disconnects as soon as the commands below run, including the phone or laptop you are using; the old Wi-Fi password stops working.** Devices without a username and password field, such as most smart-home devices, cannot rejoin at all. Install the iPhone profile from step 4 first, so you can rejoin straight away, and keep the undo command at the end of this step to hand. On the secret screen the commands stay hidden until you tick that you understand this.

On the AP (SSH):

1. Check WPA2-Enterprise support: `(opkg list-installed 2>/dev/null; apk list --installed 2>/dev/null) | grep wpad`. If it shows `wpad-basic-…`, install `wpad-mbedtls` first.
2. Save the current Wi-Fi settings: `cp /etc/config/wireless /root/wireless.before-radius`.
3. Turn on RADIUS on the existing access-point networks. Paste all lines at once; it prints each network it changed:

```sh
for s in $(uci show wireless | sed -n 's/^wireless\.\([^.=]*\)=wifi-iface$/\1/p'); do
  [ "$(uci -q get wireless.$s.mode)" = ap ] || continue
  uci set wireless.$s.encryption='wpa2'
  uci set wireless.$s.auth_server='192.168.1.10'
  uci set wireless.$s.auth_port='1812'
  uci set wireless.$s.auth_secret='<secret from step 2>'
  echo "RADIUS on: $(uci -q get wireless.$s.ssid)"
done
uci commit wireless
wifi reload
```

`192.168.1.10` is the OpenManage host. To change only some networks, use the commands from the secret screen with **Wi-Fi networks to change** filled in; they add a line that skips the other network names.

To undo: `cp /root/wireless.before-radius /etc/config/wireless; wifi reload`.

## 4. Install the profile on the iPhone

1. In the Server certificate card, type the name of the Wi-Fi network you switched (for example `Home`), pick the Wi-Fi user from step 2 (for example `nishant-iphone`) and press **Download setup profile**. With a user picked, the iPhone asks only for that user's password. With **Ask on the device**, it asks for a username too: that is the Wi-Fi user, not the network name. Open the file on the iPhone (AirDrop it, or email it to yourself).
2. Settings → **Profile Downloaded** → Install. iOS says the profile is not signed; that is expected. The profile contains the OpenManage CA, the network settings and the Wi-Fi username if you picked one, and no password.
3. Check the profile details show the CA named "OpenManage RADIUS CA ..." before installing.

## 5. Test checklist

Record pass or fail for each step. The dashboard's **Recent activity** shows `radius.auth_accept` / `radius.auth_reject` events with the username and reason.

| # | Step | Expected |
|---|------|----------|
| 1 | Join `Home`; enter the Wi-Fi user's password (and username, if asked) | Connects without a certificate warning; Safari loads a page. Audit: `radius.auth_accept`, method `eap-ttls` |
| 2 | Reset the Wi-Fi user's password in the dashboard (the phone still has the old one); turn Wi-Fi off and on | Fails. Audit: `radius.auth_reject`, reason `bad_credentials`. Then tap `Home` and enter the new password; it connects. (A network installed by a profile has no Forget button; remove the profile to forget it.) |
| 3 | Disable the Wi-Fi user in the dashboard; turn Wi-Fi off and on | Cannot join. Audit: reason `disabled`. Re-enable the user; the phone joins again within a minute |
| 4 | Turn Wi-Fi off and on, then restart the phone | Rejoins on its own without asking |
| 5 | Leave it connected for 15 minutes and browse | Stays connected (the keys the AP received are correct) |
| 6 | Optional: remove the profile and join `Home` manually from the Wi-Fi list | iOS shows a certificate prompt for `radius.openmanage.home.arpa`. Do not trust it unless the fingerprint matches the dashboard. Manual join may pick an inner method other than PAP; if it fails with reason `inner_method_not_pap`, that is a known limitation and the profile is the supported path |

If step 1 fails, check in this order:

- **No audit event at all**: the request did not reach OpenManage or was dropped. Check the dashboard's "Ignored packets" count. If it rises, the AP's source address or secret does not match (RADIUS requests must come from the AP's inventory address).
- **`tls_failed`**: the phone refused the server. Check the profile's SSID and that it was installed.
- **`bad_credentials` with the network name as username**: the username typed on the phone was the SSID. Remove the profile, download it again with the Wi-Fi user picked, and reinstall.
- **`inner_method_not_pap`**: the phone was not using the profile.
- **Audit shows accept but the phone does not connect**: the key delivery to the AP failed. Capture `logread | grep hostapd` on the AP and share it.

## 6. Clean up

Put the AP's Wi-Fi settings back (`cp /root/wireless.before-radius /etc/config/wireless; wifi reload`), and in the dashboard remove the AP's RADIUS secret. To turn RADIUS off, unset `OPENMANAGE_RADIUS_LISTEN`, remove the port mapping and restart. The CA is kept, so reinstalled profiles keep working when RADIUS is turned on again.
