package main

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// settingsExportOperation reads the full UCI configuration for the settings view.
// Unlike the snapshot export it is available to every signed-in role, because the
// response redacts secret-looking values before they leave the server.
var settingsExportOperation = operation{
	Name: "uci_export", Label: "Configuration export", Class: operationRead,
	Roles: readOnlyRoles, Timeout: healthCheckTimeout, OutputLimit: configExportLimit,
	Audit: auditBestEffort, command: "uci export",
}

type settingsOption struct {
	Name   string `json:"name"`
	Value  string `json:"value,omitempty"`
	Secret bool   `json:"secret"`
}

type settingsSection struct {
	Name    string           `json:"name"`
	Type    string           `json:"type"`
	Options []settingsOption `json:"options"`
}

type settingsPackage struct {
	// Version is a keyed digest of the package's values, for detecting that the
	// device changed after the page loaded. It is computed over the unredacted
	// values, so it also covers secret options.
	Version  string            `json:"version"`
	Sections []settingsSection `json:"sections"`
}

type deviceSettings struct {
	DeviceID   string                     `json:"device_id"`
	CapturedAt time.Time                  `json:"captured_at"`
	Packages   map[string]settingsPackage `json:"packages"`
}

// getDeviceSettings exports the device's live UCI configuration, groups it by
// package, and redacts secret-looking option values. The raw export never leaves
// the server.
func (a *app) getDeviceSettings(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("id")
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireOperationRoles(w, r, []operation{settingsExportOperation}) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		status, msg := targetError(err)
		writeError(w, status, msg)
		return
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not open a verified SSH session to the device")
		return
	}
	result := session.run(settingsExportOperation)
	session.Close()
	if result.Status != "ok" {
		writeError(w, http.StatusBadGateway, "configuration export failed: "+result.Error)
		return
	}
	cfg, err := parseUCIExport(result.Output)
	if err != nil {
		writeError(w, http.StatusBadGateway, "the device did not return a UCI configuration export")
		return
	}
	key, err := encryptionKey()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SSH operations are disabled: server encryption key is not configured")
		return
	}
	writeJSON(w, http.StatusOK, deviceSettings{
		DeviceID: deviceID, CapturedAt: time.Now().UTC(), Packages: groupSettings(cfg, key),
	})
}

// groupSettings turns a parsed UCI export into per-package sections with secret
// values redacted and a keyed version token per package.
func groupSettings(cfg uciConfig, key []byte) map[string]settingsPackage {
	types := map[string]map[string]string{}              // package -> section -> type
	options := map[string]map[string]map[string]string{} // package -> section -> option -> value
	for path, value := range cfg {
		parts := strings.Split(path, ".")
		switch len(parts) {
		case 2:
			if types[parts[0]] == nil {
				types[parts[0]] = map[string]string{}
			}
			types[parts[0]][parts[1]] = value
		case 3:
			if options[parts[0]] == nil {
				options[parts[0]] = map[string]map[string]string{}
			}
			if options[parts[0]][parts[1]] == nil {
				options[parts[0]][parts[1]] = map[string]string{}
			}
			options[parts[0]][parts[1]][parts[2]] = value
		}
	}
	pkgs := map[string]settingsPackage{}
	for pkg, sections := range types {
		sp := settingsPackage{}
		sp.Version = packageSettingsVersion(cfg, pkg, key)
		names := make([]string, 0, len(sections))
		for name := range sections {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			sec := settingsSection{Name: name, Type: sections[name]}
			opts := options[pkg][name]
			optNames := make([]string, 0, len(opts))
			for o := range opts {
				optNames = append(optNames, o)
			}
			sort.Strings(optNames)
			for _, o := range optNames {
				so := settingsOption{Name: o}
				if secretOption.MatchString(o) {
					so.Secret = true
				} else {
					so.Value = opts[o]
				}
				sec.Options = append(sec.Options, so)
			}
			sp.Sections = append(sp.Sections, sec)
		}
		pkgs[pkg] = sp
	}
	return pkgs
}

// packageSettingsVersion covers every exported path in a UCI package, including
// section types and empty sections. Length-prefixing prevents values containing
// delimiters or newlines from making distinct configurations hash identically.
func packageSettingsVersion(cfg uciConfig, pkg string, key []byte) string {
	prefix := pkg + "."
	paths := make([]string, 0)
	for path := range cfg {
		if strings.HasPrefix(path, prefix) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	var canonical strings.Builder
	canonical.WriteString(strconv.Itoa(len(paths)))
	canonical.WriteByte(':')
	for _, path := range paths {
		value := cfg[path]
		canonical.WriteString(strconv.Itoa(len(path)))
		canonical.WriteByte(':')
		canonical.WriteString(path)
		canonical.WriteString(strconv.Itoa(len(value)))
		canonical.WriteByte(':')
		canonical.WriteString(value)
	}
	return keyedDigest(key, "openmanage:settings-version:v2", canonical.String())
}
