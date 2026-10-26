# Device location map: design (draft for approval)

## Goal
On the Wi-Fi floor plan, show each client device as a dot in the room it is in: green = connected now, red = seen in the last 24 h but not connected now, gone after 24 h.

## Starting point
- #25 (draft): browser-only SVG coverage planner, local storage layout. Superseded by #26.
- #26 (draft, 43 commits, 1144 lines, CI not reviewed): persistent floor plan (rooms, walls, AP placements) in Postgres, plus manual `iwinfo` neighbour scans stored 30 days. Built on old main (6e6371b), so it needs a rebase onto current main (migrations 005-010 and the typed operation engine landed since). Its `iwinfo` ops are run as fixed commands; they should be re-declared in `operations.go`.
- Plan: rebase/reduce #26 to "floor plan + AP placement" as PR 1, then add client dots as later PRs. RSSI/heatmap parts of #26 are not needed for dots and can stay parked.

## 1. Placing a client in a room (accuracy honestly)
| Method | Needs | Realistic accuracy |
|---|---|---|
| A. Associated AP | which AP the client is on | Room-level only if each room has its own AP (or APs are in separate rooms). With one AP covering several rooms, you only learn "somewhere in the AP's zone". |
| B. Per-AP RSSI (several APs hear the client) | each AP reports signal for clients it hears (associated only, in hostapd/ubus; probe-request sniffing is not exposed) | Strongest-AP = A. Weighted centroid of 2-3 APs: roughly 3-6 m indoors, walls and phone orientation add large noise. Fine for rooms with different nearest AP, poor for adjacent rooms. |
| C. Trilateration | calibrated path loss per wall/floor | Needs a site survey; 5-8 m error typical in homes. Not worth it. |

Recommendation: **A + B-lite**. Place the dot in the room of the AP the client is associated with, and if a client is associated to one AP but a second AP in the same layout reports a materially stronger neighbour, still stay with the associated AP (clients roam to the strongest, so association already encodes that). Dot is positioned inside the AP's room, offset slightly so several clients do not stack. Explicit label: "Room is the room of the AP it is connected to". Where several rooms share one AP, the dot goes to that AP's room, and the user can optionally pin a device to a room manually ("my iPhone usually lives in Bedroom"); manual pin is only used for the disconnected (red) dot.
With Nishant's setup (home, iPhone, likely a few APs), expect correct room when each room's nearest AP is in or next to it; wrong or ambiguous in open-plan areas.
iPhone caveat: iOS "Private Wi-Fi Address" gives a per-SSID random MAC; it is stable per network, so tracking works, but resets if the network is forgotten. Phones sleeping Wi-Fi may appear disconnected while still in the room.

## 2. Data source on OpenWrt (typed ops only)
Two fixed read-only operations through the existing engine (no generic shell, bounded output/timeout):
- `wifi_ap_list`: `ubus list 'hostapd.*'` to find AP interfaces.
- `wifi_clients`: `ubus call hostapd.<iface> get_clients` for each interface returned by the allow-listed pattern `^hostapd\.[A-Za-z0-9_.-]{1,32}$` (name validated server-side, never user supplied). Returns client MAC, auth/assoc flags, signal (RSSI), rx/tx bytes.
- Device names (built): the same `wifi_clients` op also prints two capped side tables, `/tmp/dhcp.leases` (MAC and hostname) and `ip -4 neigh show` (MAC and IP). Names come from, in order: the user's label; the lease file of a device marked **DHCP host** (`PATCH /api/devices/{id}` with `dhcp_host`; leases from other devices are ignored; a router that is not an AP can name clients seen on any AP); reverse DNS (PTR) of the client's IP from the AP's neighbour table, private IPv4 only, 1 s timeout, at most 16 uncached lookups per poll, cached in memory (1 h hit, 5 min miss). Only plain hostnames are kept. A bridge AP may know few client IPs, so reverse DNS is best effort.
Run by the existing opt-in monitoring loop (same advisory lock, 3 workers), at the monitoring interval (suggest 60 s min; default 5 min), so no new service. Opt-in toggle `client_tracking_enabled` in monitoring settings, default off.

## 3. Storage and 24 h retention
One additive migration, `wifi_client_sightings`:
- `client_key` (HMAC of MAC, see 4), `device_id` (AP), `ssid`, `signal_dbm`, `first_seen`, `last_seen`, `connected bool`.
- Upsert one row per (client_key, device_id): update `last_seen` and signal while connected. When a poll no longer lists the client on that AP, mark `connected=false` (keeps `last_seen`).
- Display rule: green if connected in the latest poll; red if `last_seen` within 24 h; hidden otherwise. Prune rows with `last_seen` older than 24 h on every poll. Raw sightings history is not kept (just current state + last location), which is the smallest privacy footprint.
- Migration is additive; old app version ignores the table. Rollback loses only the transient sightings.

## 4. Privacy of MACs
- Store a keyed HMAC-SHA-256 of the MAC (key derived from `OPENMANAGE_ENCRYPTION_KEY`) as the identity; the raw MAC is stored encrypted (AES-GCM, same as snapshots) only so the owner can see and label it. API returns MAC only to admin/operator, masked (`aa:bb:cc:••:••:••`) by default; reveal on click is audited.
- Users label a device ("Nishant's iPhone"); unlabeled ones show as "Unknown device" with vendor-less masked MAC.
- Never logged, never in audit details. Feature off by default; 24 h hard cap; a "Clear client data" button deletes all rows.
- Home use caveat: guests' phones will appear as unknown dots; there is a per-device "hide" flag.

## 5. Dot behaviour
- Green: `connected` in the latest poll (age < 2x poll interval), shown at the associated AP's room. Hover: label, SSID, signal, connected-since.
- Red: not connected now, last seen < 24 h; positioned at last known AP room, tooltip "last seen 3 h ago in Bedroom".
- Hidden: last seen > 24 h, or user hid it. Dot reappears green if it reconnects.
- Roaming: when it moves between APs the dot moves to the new room on the next poll (up to one interval of lag).
- Legend shows "estimated from the connected AP, not measured position".

## PR breakdown (small, no new services)
1. Rebase floor plan from #26 onto main (layout + AP placement only, DB-persisted, audited). Replaces #25/#26.
2. Client tracking backend: ops, migration, monitoring hook, retention, tests (lab test with `get_clients`).
3. UI: dots, legend, labels, hide/clear.
4. Optional: manual room pin, weighted-centroid refinement if you want sub-room placement.
Avoids RADIUS files (radius*.go, eap.go, network_users.go, WifiSection.tsx). New code in `wifi_layout.go`, `wifi_clients.go`, `FloorPlan.tsx`; touches `monitoring.go` lightly.

## Questions for Nishant
1. Is "room of the connected AP" accurate enough, or do you want RSSI refinement later?
2. Poll interval OK at 5 min default (60 s min)?
3. OK to supersede #25/#26 by a rebased PR 1 and close them afterwards?

## Addendum: does it need distance from the AP?
Not for room-level dots. Option A (room of the associated AP) needs no distance at all. Distance only matters if you want position inside or between rooms.
- **RSSI to distance** (log-distance path loss): d = 10^((RSSI0 - RSSI)/(10n)). Indoors n varies 2.5-4 and each wall costs 3-10 dB, so error is easily 2x the true distance. Phone orientation, a hand over the antenna, and iOS power saving swing RSSI by 5-10 dB. Usable only as "near / mid / far" from one AP, not metres.
- **Multi-AP fingerprinting**: record RSSI vectors from each AP while standing in each room (calibration walk, one sample per room), then match live vectors to the closest room (k-NN). Best accuracy for room choice in a house with 2+ APs (often 80-90% room-correct after calibration), but needs the calibration step and re-calibration if APs move. Needs the same client heard by several APs; hostapd `get_clients` only reports clients associated to that AP, so for other APs we would need beacon/probe reports (802.11k) which not all phones answer. iPhones do support 802.11k/v/r neighbour reports on some APs, but via `hostapd_cli rrm_beacon_req` it is fiddly and not default-on.
- **802.11mc FTM (fine timing measurement)**: true ranging, ~1-2 m accuracy. Needs AP and client support. iPhones do not expose FTM responses to APs for third-party use, and few OpenWrt APs support FTM responder in hostapd for stock builds. Not viable for an iPhone today.

Recommendation: ship A first (no distance). Fingerprinting (calibrated, optional "train this room" button) is the realistic upgrade if A is not accurate enough. Skip FTM and pure RSSI trilateration.

## Update 2026-10-23: Nishant's answer to Q1
Start with room-of-connected-AP; more precise location (fingerprinting) is a real later goal. AP association history is also wanted for roaming tests, so the 24 h rule applies to the map dots only, not to roaming history:
- Add `wifi_client_roams` (client_key, from_device, to_device, signal_dbm, at): one row each time a client's associated AP changes, plus first connect/disconnect events. Kept 30 days (matches monitoring history), shown as a per-client timeline ("iPhone: Hall AP -> Bedroom AP, 21:04").
- Also store RSSI with each association change so later fingerprinting has data to learn from.
- Privacy: still keyed-hash identity and masked MACs; "Clear client data" wipes both tables. Roam history retention is a documented, configurable setting (default 30 days, minimum 24 h).
- Defaults: 5 min poll, adjustable from 60 s up to 10 min if 5 min is too noisy (Nishant, 2026-10-23), #25/#26 closed after the rebased floor plan lands. Note a 5 min poll can miss quick roams; roam tests may want a 60 s poll.
