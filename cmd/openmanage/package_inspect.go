package main

import (
	"archive/tar"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Reading the metadata of a package file that an administrator registers as a package
// source (docs/firmware-upgrades.md, "Package sources"). The file is untrusted input:
// every read is bounded, nothing is extracted to disk, and nothing is executed. Two
// formats are understood: .ipk (opkg: a gzip-compressed tar or an ar archive holding
// control.tar.gz) and .apk (apk-tools 3: the ADB format, deflate-compressed).

const (
	packageMaxFileBytes    = 32 << 20  // largest package file accepted
	packageMaxExpanded     = 256 << 20 // most bytes read while looking through an .ipk
	packageMaxControlBytes = 1 << 20   // control.tar.gz, expanded
	packageMaxFieldBytes   = 64 << 10  // control file or one script
	packageMaxADBBytes     = 4 << 20   // first ADB block (metadata, file list, scripts)
	packageMaxDepends      = 256
)

// The name and version patterns are shared with the firmware inventory.
var packageArchPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

type packageScript struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// packageInfo is what the administrator reviews before a source is confirmed. Text taken
// from the file is shown as data and never interpreted.
type packageInfo struct {
	Format        string          `json:"format"` // ipk or apk
	Name          string          `json:"name"`
	Version       string          `json:"version"`
	Arch          string          `json:"arch"`
	Description   string          `json:"description"`
	Depends       []string        `json:"depends"`
	Scripts       []packageScript `json:"scripts"`
	InstalledSize int64           `json:"installed_size"`
}

var errPackageUnsupported = errors.New("this is not a supported .ipk or .apk package file")

// inspectPackage detects the format from the file's first bytes and reads its metadata.
func inspectPackage(data []byte) (packageInfo, error) {
	if len(data) == 0 || len(data) > packageMaxFileBytes {
		return packageInfo{}, errors.New("the package file is empty or too large")
	}
	var info packageInfo
	var err error
	switch {
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		info, err = inspectIPK(data)
	case bytes.HasPrefix(data, []byte("!<arch>\n")):
		info, err = inspectIPK(data)
	case bytes.HasPrefix(data, []byte("ADBd")), bytes.HasPrefix(data, []byte("ADB.")):
		info, err = inspectAPK(data)
	default:
		return packageInfo{}, errPackageUnsupported
	}
	if err != nil {
		return packageInfo{}, err
	}
	switch {
	case !packageNamePattern.MatchString(info.Name):
		return packageInfo{}, errors.New("the package name is missing or not a valid package name")
	case !packageVersionPattern.MatchString(info.Version):
		return packageInfo{}, errors.New("the package version is missing or not valid")
	case !packageArchPattern.MatchString(info.Arch):
		return packageInfo{}, errors.New("the package architecture is missing or not valid")
	}
	if info.Depends == nil {
		info.Depends = []string{}
	}
	if info.Scripts == nil {
		info.Scripts = []packageScript{}
	}
	info.Description = cleanPackageText(info.Description, 512)
	return info, nil
}

// cleanPackageText makes text from a package safe and honest to show: invalid UTF-8,
// control characters (other than newline and tab) and invisible formatting characters
// such as bidirectional overrides are written as visible escapes instead of being
// dropped, so what the administrator reads is what the device would run. The result is
// cut to about max bytes.
func cleanPackageText(s string, max int) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < max; {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029:
			fmt.Fprintf(&b, `\u{%04X}`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return strings.TrimSpace(b.String())
}

var errScriptTooLarge = errors.New("an install script in this package is larger than 64 KiB, too large to review")

// scriptFrom keeps the whole script: one that is too large to show in full is refused,
// so no part of a script that runs as root goes unseen.
func scriptFrom(name string, body []byte) (packageScript, error) {
	if len(body) > packageMaxFieldBytes {
		return packageScript{}, errScriptTooLarge
	}
	return packageScript{Name: name, Body: cleanPackageText(string(body), 4*packageMaxFieldBytes+64)}, nil
}

// --- .ipk ------------------------------------------------------------------------------

func inspectIPK(data []byte) (packageInfo, error) {
	control, err := ipkControlArchive(data)
	if err != nil {
		return packageInfo{}, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(control))
	if err != nil {
		return packageInfo{}, errors.New("the package's control archive is not gzip-compressed, which is not supported")
	}
	tr := tar.NewReader(io.LimitReader(gz, packageMaxControlBytes))
	info := packageInfo{Format: "ipk"}
	haveControl := false
	scripts := map[string]packageScript{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return packageInfo{}, errors.New("the package's control archive is damaged")
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		switch name {
		case "control", "preinst", "postinst", "prerm", "postrm":
		default:
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, packageMaxFieldBytes+1))
		if err != nil || len(body) > packageMaxFieldBytes {
			return packageInfo{}, errors.New("the package's control data is damaged or too large")
		}
		if name == "control" {
			parseIPKControl(string(body), &info)
			haveControl = true
		} else {
			script, err := scriptFrom(name, body)
			if err != nil {
				return packageInfo{}, err
			}
			scripts[name] = script
		}
	}
	if !haveControl {
		return packageInfo{}, errors.New("the package has no control file")
	}
	for _, n := range []string{"preinst", "postinst", "prerm", "postrm"} {
		if s, ok := scripts[n]; ok {
			info.Scripts = append(info.Scripts, s)
		}
	}
	return info, nil
}

// ipkControlArchive returns the bytes of control.tar.gz from an .ipk, which is either a
// gzip-compressed tar (what OpenWrt builds) or an ar archive (what Debian tools build).
func ipkControlArchive(data []byte) ([]byte, error) {
	if bytes.HasPrefix(data, []byte("!<arch>\n")) {
		return arMember(data, "control.tar.gz")
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, errPackageUnsupported
	}
	tr := tar.NewReader(io.LimitReader(gz, packageMaxExpanded))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the package has no control archive")
		}
		if err != nil {
			return nil, errors.New("the package file is damaged or not an .ipk")
		}
		if strings.TrimPrefix(hdr.Name, "./") != "control.tar.gz" {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, packageMaxControlBytes+1))
		if err != nil || len(body) > packageMaxControlBytes {
			return nil, errors.New("the package's control archive is damaged or too large")
		}
		return body, nil
	}
}

func arMember(data []byte, want string) ([]byte, error) {
	pos := 8
	for pos+60 <= len(data) {
		name := strings.TrimRight(strings.TrimSpace(string(data[pos:pos+16])), "/")
		size, err := strconv.Atoi(strings.TrimSpace(string(data[pos+48 : pos+58])))
		if err != nil || size < 0 || data[pos+58] != '`' || data[pos+59] != '\n' {
			return nil, errors.New("the package file is damaged or not an .ipk")
		}
		pos += 60
		if size > len(data)-pos {
			return nil, errors.New("the package file is truncated")
		}
		if name == want {
			if size > packageMaxControlBytes {
				return nil, errors.New("the package's control archive is too large")
			}
			return data[pos : pos+size], nil
		}
		pos += size + size%2
	}
	return nil, errors.New("the package has no control archive")
}

// parseIPKControl reads the "Key: value" lines of a control file; a line starting with a
// space continues the previous value.
func parseIPKControl(text string, info *packageInfo) {
	fields := map[string]string{}
	key := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if key != "" {
				fields[key] += "\n" + strings.TrimSpace(line)
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			key = ""
			continue
		}
		key = strings.ToLower(strings.TrimSpace(k))
		fields[key] = strings.TrimSpace(v)
	}
	info.Name = fields["package"]
	info.Version = fields["version"]
	info.Arch = fields["architecture"]
	info.Description = fields["description"]
	if n, err := strconv.ParseInt(fields["installed-size"], 10, 64); err == nil && n >= 0 {
		info.InstalledSize = n
	}
	for _, dep := range strings.Split(fields["depends"], ",") {
		if dep = cleanPackageText(dep, 128); dep != "" && len(info.Depends) < packageMaxDepends {
			info.Depends = append(info.Depends, dep)
		}
	}
}

// --- .apk (apk-tools 3, ADB) ------------------------------------------------------------

// An ADB file is the magic "ADB." (or "ADBd" when the rest is raw-deflate compressed), a
// four-byte schema name ("pckg" for a package), then blocks. The first block holds the
// metadata: a table of 32-bit values whose top four bits give the type and the rest an
// offset (or a small integer), relative to the block's payload.
const (
	adbTypeInt    = 0x1
	adbTypeInt32  = 0x2
	adbTypeBlob8  = 0x8
	adbTypeBlob16 = 0x9
	adbTypeBlob32 = 0xa
	adbTypeArray  = 0xd
	adbTypeObject = 0xe
)

type adbBlock struct{ p []byte }

func (b adbBlock) u32(off int) (uint32, bool) {
	if off < 0 || off+4 > len(b.p) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b.p[off:]), true
}

func (b adbBlock) blob(v uint32) ([]byte, bool) {
	off := int(v & 0x0fffffff)
	var n int
	switch v >> 28 {
	case adbTypeBlob8:
		if off+1 > len(b.p) {
			return nil, false
		}
		n, off = int(b.p[off]), off+1
	case adbTypeBlob16:
		if off+2 > len(b.p) {
			return nil, false
		}
		n, off = int(binary.LittleEndian.Uint16(b.p[off:])), off+2
	case adbTypeBlob32:
		l, good := b.u32(off)
		if !good {
			return nil, false
		}
		n, off = int(l), off+4
	default:
		return nil, false
	}
	if n < 0 || off+n > len(b.p) {
		return nil, false
	}
	return b.p[off : off+n], true
}

// values returns the fields of an object or array value (the count word is skipped).
func (b adbBlock) values(v uint32) ([]uint32, bool) {
	if t := v >> 28; t != adbTypeObject && t != adbTypeArray {
		return nil, false
	}
	off := int(v & 0x0fffffff)
	n, ok := b.u32(off)
	if !ok || n < 1 || n > 4096 || off+int(n)*4 > len(b.p) {
		return nil, false
	}
	out := make([]uint32, 0, n-1)
	for i := 1; i < int(n); i++ {
		w, _ := b.u32(off + 4*i)
		out = append(out, w)
	}
	return out, true
}

func (b adbBlock) text(v uint32) string {
	body, _ := b.blob(v)
	return string(body)
}

func (b adbBlock) integer(v uint32) int64 {
	switch v >> 28 {
	case adbTypeInt:
		return int64(v & 0x0fffffff)
	case adbTypeInt32:
		if n, ok := b.u32(int(v & 0x0fffffff)); ok {
			return int64(n)
		}
	}
	return 0
}

func at(vals []uint32, i int) uint32 {
	if i < len(vals) {
		return vals[i]
	}
	return 0
}

// Field positions in the package info object and the scripts object (apk-tools adb.h).
const (
	adbInfoName          = 0
	adbInfoVersion       = 1
	adbInfoDescription   = 3
	adbInfoArch          = 4
	adbInfoInstalledSize = 11
	adbInfoDepends       = 14
)

var adbScriptNames = []string{"trigger", "preinst", "postinst", "prerm", "postrm", "pre-upgrade", "post-upgrade"}

func inspectAPK(data []byte) (packageInfo, error) {
	var r io.Reader
	if bytes.HasPrefix(data, []byte("ADBd")) {
		// The compressed stream starts again with the plain magic.
		r = flate.NewReader(bytes.NewReader(data[4:]))
		magic := make([]byte, 4)
		if _, err := io.ReadFull(r, magic); err != nil || string(magic) != "ADB." {
			return packageInfo{}, errors.New("this .apk is damaged or not in a supported format")
		}
	} else {
		r = bytes.NewReader(data[4:])
	}
	head := make([]byte, 8+4)
	if _, err := io.ReadFull(r, head); err != nil || string(head[:4]) != "pckg" {
		return packageInfo{}, errors.New("this .apk is not a package (unexpected schema) or is damaged")
	}
	blockHdr := binary.LittleEndian.Uint32(head[4:8])
	size := int(blockHdr & 0x3fffffff)
	if blockHdr>>30 != 0 || size < 4+8 || size > packageMaxADBBytes {
		return packageInfo{}, errors.New("this .apk has an unexpected metadata block, or it is too large")
	}
	payload := make([]byte, size-4)
	copy(payload, head[8:12])
	if _, err := io.ReadFull(r, payload[4:]); err != nil {
		return packageInfo{}, errors.New("this .apk is damaged or truncated")
	}
	b := adbBlock{p: payload}
	rootWord, _ := b.u32(4)
	root, ok := b.values(rootWord)
	if !ok {
		return packageInfo{}, errors.New("this .apk's metadata cannot be read")
	}
	infoVals, ok := b.values(at(root, 0))
	if !ok {
		return packageInfo{}, errors.New("this .apk has no package information")
	}
	info := packageInfo{
		Format:        "apk",
		Name:          b.text(at(infoVals, adbInfoName)),
		Version:       b.text(at(infoVals, adbInfoVersion)),
		Description:   b.text(at(infoVals, adbInfoDescription)),
		Arch:          b.text(at(infoVals, adbInfoArch)),
		InstalledSize: b.integer(at(infoVals, adbInfoInstalledSize)),
	}
	if deps, ok := b.values(at(infoVals, adbInfoDepends)); ok {
		for _, d := range deps {
			parts, ok := b.values(d)
			if !ok || len(info.Depends) >= packageMaxDepends {
				continue
			}
			dep := cleanPackageText(b.text(at(parts, 0)), 128)
			if v := cleanPackageText(b.text(at(parts, 1)), 64); v != "" {
				dep += " (" + v + ")"
			}
			if dep != "" {
				info.Depends = append(info.Depends, dep)
			}
		}
	}
	if scripts, ok := b.values(at(root, 2)); ok {
		for i, v := range scripts {
			if i >= len(adbScriptNames) {
				break
			}
			if body, ok := b.blob(v); ok && len(body) > 0 {
				script, err := scriptFrom(adbScriptNames[i], body)
				if err != nil {
					return packageInfo{}, err
				}
				info.Scripts = append(info.Scripts, script)
			}
		}
	}
	if info.Name == "" {
		return packageInfo{}, fmt.Errorf("this .apk has no package name")
	}
	return info, nil
}
