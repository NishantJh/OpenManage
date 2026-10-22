package main

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// uciConfig maps UCI paths to values, the way `uci show` names them:
// "network.lan" is a section's type, "network.lan.proto" an option, and anonymous
// sections are numbered per type ("wireless.@wifi-iface[0].ssid"). A list option
// joins its values with "\n", in order.
type uciConfig map[string]string

var errNotUCIExport = errors.New("output is not a UCI export")

// parseUCIExport reads `uci export` output. Quoting follows the shell rules uci
// uses when exporting ('it'\”s' is "it's").
func parseUCIExport(text string) (uciConfig, error) {
	cfg := uciConfig{}
	pkg, section := "", ""
	anon := map[string]int{}
	for _, line := range strings.Split(text, "\n") {
		fields, err := uciFields(line)
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "package":
			if len(fields) != 2 {
				return nil, errNotUCIExport
			}
			pkg, section = fields[1], ""
			anon = map[string]int{}
		case "config":
			if pkg == "" || len(fields) < 2 || len(fields) > 3 {
				return nil, errNotUCIExport
			}
			typ := fields[1]
			if len(fields) == 3 {
				section = fields[2]
			} else {
				section = "@" + typ + "[" + strconv.Itoa(anon[typ]) + "]"
				anon[typ]++
			}
			cfg[pkg+"."+section] = typ
		case "option", "list":
			if section == "" || len(fields) != 3 {
				return nil, errNotUCIExport
			}
			key := pkg + "." + section + "." + fields[1]
			if prev, ok := cfg[key]; ok && fields[0] == "list" {
				cfg[key] = prev + "\n" + fields[2]
			} else {
				cfg[key] = fields[2]
			}
		default:
			return nil, errNotUCIExport
		}
	}
	if len(cfg) == 0 {
		return nil, errNotUCIExport
	}
	return cfg, nil
}

// uciFields splits one line into words, honouring single quotes, double quotes, and
// backslash escapes. A '#' at the start of a word begins a comment.
func uciFields(line string) ([]string, error) {
	var fields []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			if inWord {
				fields = append(fields, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '#' && !inWord:
			return fields, nil
		case c == '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				return nil, errNotUCIExport
			}
			cur.WriteString(line[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			j := i + 1
			for ; j < len(line) && line[j] != '"'; j++ {
				if line[j] == '\\' && j+1 < len(line) {
					j++
				}
				cur.WriteByte(line[j])
			}
			if j >= len(line) {
				return nil, errNotUCIExport
			}
			i = j
			inWord = true
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			inWord = true
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		fields = append(fields, cur.String())
	}
	return fields, nil
}

type configChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // added, removed, changed
	Old  string `json:"old,omitempty"`
	New  string `json:"new,omitempty"`
	// Redacted is true when the option looks secret; Old and New are then omitted.
	Redacted bool `json:"redacted,omitempty"`
}

// secretOption matches option names whose values must never leave the server in a
// diff: Wi-Fi keys, passwords, RADIUS secrets, private keys, tokens.
var secretOption = regexp.MustCompile(`(?i)(key|pass|secret|psk|token|private|auth)`)

// diffUCI lists every path that differs, sorted, with secret-looking values redacted.
func diffUCI(before, after uciConfig) []configChange {
	changes := []configChange{}
	add := func(path, kind, old, new string) {
		c := configChange{Path: path, Kind: kind, Old: old, New: new}
		if parts := strings.Split(path, "."); len(parts) == 3 && secretOption.MatchString(parts[2]) {
			c.Old, c.New, c.Redacted = "", "", true
		}
		changes = append(changes, c)
	}
	for path, old := range before {
		if new, ok := after[path]; !ok {
			add(path, "removed", old, "")
		} else if new != old {
			add(path, "changed", old, new)
		}
	}
	for path, new := range after {
		if _, ok := before[path]; !ok {
			add(path, "added", "", new)
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}
