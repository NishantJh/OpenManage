package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"unicode/utf8"
)

// Apple configuration profile (.mobileconfig) for iPhone, iPad and Mac. It installs
// the RADIUS CA as a trust anchor and a Wi-Fi network that uses EAP-TTLS with inner
// PAP and accepts only OpenManage's server name, so the device never has to ask the
// user to trust an unknown certificate. The profile is unsigned and holds no
// password. If a Wi-Fi username is given it is preset, so the device asks only for
// the password; otherwise it asks for both when joining.

func validSSID(ssid string) bool {
	if len(ssid) == 0 || len(ssid) > 32 || !utf8.ValidString(ssid) {
		return false
	}
	for _, r := range ssid {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// profileUUID derives a stable UUID-shaped identifier, so reinstalling a profile for
// the same CA and SSID replaces the earlier one instead of adding a duplicate.
func profileUUID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	s := h.Sum(nil)
	return fmt.Sprintf("%X-%X-%X-%X-%X", s[0:4], s[4:6], s[6:8], s[8:10], s[10:16])
}

func xmlText(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func appleWiFiProfile(caDER []byte, ssid, username string) []byte {
	fp := certFingerprint(caDER)
	caUUID := profileUUID("ca", fp)
	wifiUUID := profileUUID("wifi", fp, ssid)
	profile := profileUUID("profile", fp, ssid)
	userName := ""
	if username != "" {
		userName = "\n\t\t\t\t<key>UserName</key><string>" + xmlText(username) + "</string>"
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>PayloadContent</key>
	<array>
		<dict>
			<key>PayloadType</key><string>com.apple.security.root</string>
			<key>PayloadVersion</key><integer>1</integer>
			<key>PayloadIdentifier</key><string>arpa.home.openmanage.radius.ca.%[1]s</string>
			<key>PayloadUUID</key><string>%[1]s</string>
			<key>PayloadDisplayName</key><string>OpenManage RADIUS CA</string>
			<key>PayloadCertificateFileName</key><string>openmanage-radius-ca.cer</string>
			<key>PayloadContent</key><data>%[2]s</data>
		</dict>
		<dict>
			<key>PayloadType</key><string>com.apple.wifi.managed</string>
			<key>PayloadVersion</key><integer>1</integer>
			<key>PayloadIdentifier</key><string>arpa.home.openmanage.radius.wifi.%[3]s</string>
			<key>PayloadUUID</key><string>%[3]s</string>
			<key>PayloadDisplayName</key><string>Wi-Fi (%[4]s)</string>
			<key>SSID_STR</key><string>%[4]s</string>
			<key>EncryptionType</key><string>WPA2</string>
			<key>AutoJoin</key><true/>
			<key>HIDDEN_NETWORK</key><false/>
			<key>EAPClientConfiguration</key>
			<dict>
				<key>AcceptEAPTypes</key><array><integer>21</integer></array>
				<key>TTLSInnerAuthentication</key><string>PAP</string>
				<key>OuterIdentity</key><string>anonymous</string>
				<key>PayloadCertificateAnchorUUID</key><array><string>%[1]s</string></array>
				<key>TLSTrustedServerNames</key><array><string>%[5]s</string></array>
				<key>TLSAllowTrustExceptions</key><false/>
				<key>TLSMaximumVersion</key><string>1.2</string>%[8]s
			</dict>
		</dict>
	</array>
	<key>PayloadType</key><string>Configuration</string>
	<key>PayloadVersion</key><integer>1</integer>
	<key>PayloadIdentifier</key><string>arpa.home.openmanage.radius.%[6]s</string>
	<key>PayloadUUID</key><string>%[6]s</string>
	<key>PayloadDisplayName</key><string>OpenManage Wi-Fi: %[4]s</string>
	<key>PayloadDescription</key><string>Joins %[4]s with your Wi-Fi username and password, trusting only the OpenManage RADIUS server %[5]s (CA %[7]s).</string>
	<key>PayloadOrganization</key><string>OpenManage</string>
	<key>PayloadRemovalDisallowed</key><false/>
</dict>
</plist>
`, caUUID, base64.StdEncoding.EncodeToString(caDER), wifiUUID, xmlText(ssid), radiusServerName, profile, fp, userName))
}

// downloadAppleWiFiProfile serves the profile for ?ssid=<network name>, with an
// optional &username=<Wi-Fi user> to preset.
func (a *app) downloadAppleWiFiProfile(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	ssid := r.URL.Query().Get("ssid")
	if !validSSID(ssid) {
		writeError(w, http.StatusBadRequest, "ssid must be 1-32 bytes of printable text")
		return
	}
	username := r.URL.Query().Get("username")
	if username != "" && !validUsername.MatchString(username) {
		writeError(w, http.StatusBadRequest, "username is not a valid Wi-Fi username")
		return
	}
	info, caDER, err := loadRADIUSCertInfo(r.Context(), a.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the RADIUS certificate")
		return
	}
	if info == nil {
		writeError(w, http.StatusNotFound, "RADIUS has not been enabled yet, so there is no certificate")
		return
	}
	w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	w.Header().Set("Content-Disposition", `attachment; filename="openmanage-wifi.mobileconfig"`)
	w.Write(appleWiFiProfile(caDER, ssid, username))
}
