package main

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// The settings schema registry describes every UCI option OpenManage can show and,
// later, change. The browser renders forms from GET /api/config-schema; the server
// re-validates every value before a change is previewed. Fields outside this
// registry can only be edited through the admin raw editor (not yet implemented).

type settingKind string

const (
	kindString   settingKind = "string"
	kindEnum     settingKind = "enum"
	kindBool     settingKind = "bool"
	kindInt      settingKind = "int"
	kindHostname settingKind = "hostname"
	kindIPv4     settingKind = "ipv4"
	kindList     settingKind = "list"
	kindSecret   settingKind = "secret"
)

// riskClass says how disruptive a change to this field is. Disruptive changes need
// an explicit opt-in when applied; management-class changes additionally need typed
// device-name confirmation and a reachability preflight, because they can cut the
// path OpenManage uses to reach the device.
type riskClass string

const (
	riskSafe       riskClass = "safe"
	riskDisruptive riskClass = "disruptive"
	riskManagement riskClass = "management"
)

type settingField struct {
	Key            string      `json:"key"`
	Label          string      `json:"label"`
	Kind           settingKind `json:"kind"`
	Editable       bool        `json:"editable"`
	Options        []string    `json:"options,omitempty"`
	Min            *int        `json:"min,omitempty"`
	Max            *int        `json:"max,omitempty"`
	Secret         bool        `json:"secret"`
	UnlockRequired bool        `json:"unlock_required"`
	Risk           riskClass   `json:"risk"`
	Help           string      `json:"help"`
	Config         string      `json:"config"`
	Section        string      `json:"section"`
	Option         string      `json:"option"`
	// validate returns a problem, or "" when the value is acceptable. It is not
	// serialized; the browser only sees the metadata above.
	validate func(string) string
}

type settingsPage struct {
	Key      string         `json:"key"`
	Label    string         `json:"label"`
	Group    string         `json:"group"`
	Packages []string       `json:"packages"`
	Fields   []settingField `json:"fields"`
}

func intPtr(v int) *int { return &v }

func field(key, label, config, section, option string, kind settingKind, risk riskClass, help string, validate func(string) string) settingField {
	return settingField{Key: key, Label: label, Kind: kind, Risk: risk, Help: help, Config: config, Section: section, Option: option, validate: validate}
}

func enumField(key, label, config, section, option string, risk riskClass, help string, options ...string) settingField {
	f := field(key, label, config, section, option, kindEnum, risk, help, func(v string) string {
		for _, o := range options {
			if v == o {
				return ""
			}
		}
		return label + " must be one of " + strings.Join(options, ", ")
	})
	f.Options = append([]string(nil), options...)
	return f
}

func stringField(key, label, config, section, option string, risk riskClass, help string) settingField {
	return field(key, label, config, section, option, kindString, risk, help, func(v string) string {
		if len(v) > 1024 || strings.ContainsAny(v, "\x00\r\n") {
			return label + " must be at most 1024 characters and cannot contain line breaks"
		}
		return ""
	})
}

func ssidField(key, label, config, section, option string, risk riskClass, help string) settingField {
	f := field(key, label, config, section, option, kindString, risk, help, func(v string) string {
		if len(v) < 1 || len(v) > 32 || strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return "SSID must be 1-32 bytes and cannot contain control characters"
		}
		return ""
	})
	f.UnlockRequired = true
	return f
}

func channelField(key, label, config, section, option string, risk riskClass, help string) settingField {
	return field(key, label, config, section, option, kindString, risk, help, func(v string) string {
		if v == "auto" {
			return ""
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 233 || strconv.Itoa(n) != v {
			return "channel must be 'auto' or a number from 1 to 233"
		}
		return ""
	})
}

func intField(key, label, config, section, option string, min, max int, risk riskClass, help string) settingField {
	f := field(key, label, config, section, option, kindInt, risk, help, func(v string) string {
		n, err := strconv.Atoi(v)
		if err != nil || n < min || n > max || strconv.Itoa(n) != v {
			return label + " must be a whole number between " + strconv.Itoa(min) + " and " + strconv.Itoa(max)
		}
		return ""
	})
	f.Min, f.Max = intPtr(min), intPtr(max)
	return f
}

func boolField(key, label, config, section, option string, risk riskClass, help string) settingField {
	return field(key, label, config, section, option, kindBool, risk, help, func(v string) string {
		if v != "0" && v != "1" {
			return label + " must be 0 or 1"
		}
		return ""
	})
}

func hostnameField(key, label, config, section, option string, risk riskClass, help string) settingField {
	return field(key, label, config, section, option, kindHostname, risk, help, func(v string) string {
		if !hostnamePattern.MatchString(v) {
			return "hostname must be 1-63 letters, digits and hyphens, and cannot start or end with a hyphen"
		}
		return ""
	})
}

func ipv4Field(key, label, config, section, option string, risk riskClass, help string) settingField {
	return field(key, label, config, section, option, kindIPv4, risk, help, func(v string) string {
		ip := net.ParseIP(v)
		if ip == nil || ip.To4() == nil {
			return label + " must be an IPv4 address"
		}
		return ""
	})
}

func listField(key, label, config, section, option string, risk riskClass, help string, item func(string) string) settingField {
	return field(key, label, config, section, option, kindList, risk, help, func(v string) string {
		for _, line := range strings.Split(v, "\n") {
			if line == "" {
				continue
			}
			if msg := item(line); msg != "" {
				return msg
			}
		}
		return ""
	})
}

func secretField(key, label, config, section, option string, risk riskClass, help string) settingField {
	f := field(key, label, config, section, option, kindSecret, risk, help, func(v string) string {
		if len(v) < 8 || len(v) > 63 {
			return label + " must be 8-63 characters"
		}
		return ""
	})
	f.Secret = true
	return f
}

// settingsPages is the LuCI-like menu. Each page lists the UCI packages it shows and
// the curated fields OpenManage understands; the read-only view also shows every
// other option in those packages, redacted.
var settingsPages = []settingsPage{
	{
		Key: "overview", Label: "Overview", Group: "Status", Packages: []string{"system"},
		Fields: []settingField{
			hostnameField("system.hostname", "Hostname", "system", "@system[0]", "hostname", riskSafe, "1-63 letters, digits and hyphens; no leading or trailing hyphen."),
			stringField("system.timezone", "Timezone", "system", "@system[0]", "timezone", riskSafe, "POSIX timezone string, e.g. UTC or EST5EDT."),
			stringField("system.zonename", "Zone name", "system", "@system[0]", "zonename", riskSafe, "IANA zone name, e.g. Asia/Kolkata."),
			boolField("system.ntp.enabled", "NTP enabled", "system", "ntp", "enabled", riskSafe, "Synchronise the clock over the network."),
			listField("system.ntp.server", "NTP servers", "system", "ntp", "server", riskSafe, "One hostname or IP per line.", func(v string) string { return "" }),
		},
	},
	{
		Key: "system", Label: "System", Group: "System", Packages: []string{"system", "dropbear"},
		Fields: []settingField{
			hostnameField("system.hostname", "Hostname", "system", "@system[0]", "hostname", riskSafe, "1-63 letters, digits and hyphens; no leading or trailing hyphen."),
			stringField("system.timezone", "Timezone", "system", "@system[0]", "timezone", riskSafe, "POSIX timezone string, e.g. UTC or EST5EDT."),
			stringField("system.zonename", "Zone name", "system", "@system[0]", "zonename", riskSafe, "IANA zone name, e.g. Asia/Kolkata."),
			boolField("system.ntp.enabled", "NTP enabled", "system", "ntp", "enabled", riskSafe, "Synchronise the clock over the network."),
			listField("system.ntp.server", "NTP servers", "system", "ntp", "server", riskSafe, "One hostname or IP per line.", func(v string) string { return "" }),
			enumField("dropbear.password_auth", "Password login", "dropbear", "@dropbear[0]", "PasswordAuth", riskManagement, "Allow password login over SSH. Disabling it can lock out management access.", "on", "off"),
			enumField("dropbear.root_password_auth", "Root password login", "dropbear", "@dropbear[0]", "RootPasswordAuth", riskManagement, "Allow root to log in with a password over SSH.", "on", "off"),
			intField("dropbear.port", "SSH port", "dropbear", "@dropbear[0]", "Port", 1, 65535, riskManagement, "Port dropbear listens on."),
		},
	},
	{
		Key: "interfaces", Label: "Interfaces", Group: "Network", Packages: []string{"network"},
		Fields: []settingField{
			enumField("network.lan.proto", "LAN protocol", "network", "lan", "proto", riskManagement, "How the LAN interface gets its address.", "static", "dhcp", "pppoe"),
			ipv4Field("network.lan.ipaddr", "LAN IPv4 address", "network", "lan", "ipaddr", riskManagement, "Static IPv4 address of the LAN interface."),
			ipv4Field("network.lan.netmask", "LAN netmask", "network", "lan", "netmask", riskManagement, "Netmask of the LAN interface."),
			ipv4Field("network.lan.gateway", "LAN gateway", "network", "lan", "gateway", riskManagement, "Default gateway for the LAN interface."),
			listField("network.lan.dns", "LAN DNS servers", "network", "lan", "dns", riskDisruptive, "One IPv4 address per line.", func(v string) string {
				ip := net.ParseIP(v)
				if ip == nil || ip.To4() == nil {
					return "DNS servers must be IPv4 addresses"
				}
				return ""
			}),
			enumField("network.wan.proto", "WAN protocol", "network", "wan", "proto", riskManagement, "How the WAN interface gets its address.", "static", "dhcp", "pppoe"),
		},
	},
	{
		Key: "wireless", Label: "Wireless", Group: "Network", Packages: []string{"wireless"},
		Fields: []settingField{
			channelField("wireless.radio0.channel", "Channel", "wireless", "radio0", "channel", riskDisruptive, "Radio channel, or 'auto'."),
			enumField("wireless.radio0.htmode", "HT mode", "wireless", "radio0", "htmode", riskDisruptive, "802.11 mode of the radio.", "HT20", "HT40", "VHT20", "VHT40", "VHT80", "HE20", "HE40", "HE80"),
			enumField("wireless.radio0.band", "Band", "wireless", "radio0", "band", riskDisruptive, "Frequency band of the radio.", "2g", "5g", "6g"),
			ssidField("wireless.@wifi-iface[0].ssid", "SSID", "wireless", "@wifi-iface[0]", "ssid", riskDisruptive, "Network name broadcast by this interface."),
			enumField("wireless.@wifi-iface[0].encryption", "Encryption", "wireless", "@wifi-iface[0]", "encryption", riskDisruptive, "Wireless security mode.", "none", "psk2", "psk2+ccmp", "sae", "sae-mixed", "wpa2", "wpa3"),
			secretField("wireless.@wifi-iface[0].key", "Passphrase", "wireless", "@wifi-iface[0]", "key", riskDisruptive, "8-63 characters. Shown once and never logged."),
			boolField("wireless.@wifi-iface[0].disabled", "Interface enabled", "wireless", "@wifi-iface[0]", "disabled", riskDisruptive, "0 enables the interface, 1 disables it."),
		},
	},
	{
		Key: "dhcp", Label: "DHCP and DNS", Group: "Network", Packages: []string{"dhcp"},
		Fields: []settingField{
			stringField("dhcp.dnsmasq.domain", "Domain", "dhcp", "dnsmasq", "domain", riskSafe, "DNS search domain handed out by dnsmasq."),
			stringField("dhcp.lan.interface", "DHCP interface", "dhcp", "lan", "interface", riskDisruptive, "Interface the LAN DHCP pool serves."),
			intField("dhcp.lan.start", "Start", "dhcp", "lan", "start", 0, 254, riskDisruptive, "First address of the DHCP pool."),
			intField("dhcp.lan.limit", "Limit", "dhcp", "lan", "limit", 1, 254, riskDisruptive, "Number of addresses in the DHCP pool."),
			stringField("dhcp.lan.leasetime", "Lease time", "dhcp", "lan", "leasetime", riskDisruptive, "Lease duration, e.g. 12h."),
		},
	},
	{
		Key: "firewall", Label: "Firewall", Group: "Network", Packages: []string{"firewall"},
		Fields: []settingField{
			enumField("firewall.@zone[0].input", "LAN zone input", "firewall", "@zone[0]", "input", riskManagement, "Default policy for traffic entering the LAN zone.", "ACCEPT", "REJECT", "DROP"),
			enumField("firewall.@zone[0].output", "LAN zone output", "firewall", "@zone[0]", "output", riskManagement, "Default policy for traffic leaving the LAN zone.", "ACCEPT", "REJECT", "DROP"),
			enumField("firewall.@zone[0].forward", "LAN zone forward", "firewall", "@zone[0]", "forward", riskDisruptive, "Default policy for traffic forwarded through the LAN zone.", "ACCEPT", "REJECT", "DROP"),
			enumField("firewall.@zone[1].input", "WAN zone input", "firewall", "@zone[1]", "input", riskManagement, "Default policy for traffic entering the WAN zone.", "ACCEPT", "REJECT", "DROP"),
			enumField("firewall.@zone[1].output", "WAN zone output", "firewall", "@zone[1]", "output", riskManagement, "Default policy for traffic leaving the WAN zone.", "ACCEPT", "REJECT", "DROP"),
			enumField("firewall.@zone[1].forward", "WAN zone forward", "firewall", "@zone[1]", "forward", riskDisruptive, "Default policy for traffic forwarded through the WAN zone.", "ACCEPT", "REJECT", "DROP"),
		},
	},
	{
		Key: "software", Label: "Software", Group: "Software", Packages: []string{},
		Fields: []settingField{},
	},
	{
		Key: "advanced", Label: "Raw UCI", Group: "Advanced", Packages: []string{},
		Fields: []settingField{},
	},
}

// getConfigSchema serves the settings schema to the browser. It is metadata only;
// nothing here can change a device.
func (a *app) getConfigSchema(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator", "readonly") {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	pages := make([]settingsPage, len(settingsPages))
	for i, page := range settingsPages {
		pages[i] = page
		pages[i].Fields = append([]settingField(nil), page.Fields...)
		for j := range pages[i].Fields {
			pages[i].Fields[j].Editable = user.Role == "admin" && batchEditableField(pages[i].Fields[j])
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pages": pages})
}

// batchEditableField is deliberately narrower than the read-only schema. Secret
// fields stay locked pending encrypted changeset storage, while management-path
// fields need a separate typed-confirmation and reachability workflow.
func batchEditableField(f settingField) bool {
	if f.validate == nil || f.Secret || f.Risk == riskManagement || secretOption.MatchString(f.Option) || secretOption.MatchString(f.Key) {
		return false
	}
	switch f.Kind {
	case kindString, kindEnum, kindBool, kindInt, kindHostname, kindIPv4:
		return true
	default:
		return false
	}
}

func settingsField(key string) (settingField, bool) {
	for _, page := range settingsPages {
		for _, f := range page.Fields {
			if f.Key == key {
				return f, true
			}
		}
	}
	return settingField{}, false
}
