package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"golang.org/x/crypto/ssh"
)

// A sysupgrade archive is untrusted bytes: the device that produced it may be
// compromised. The server never extracts it. inspectBackupArchive reads it in memory
// with hard limits, writes nothing to disk, and returns only what OpenManage needs.

const (
	archiveMaxCompressed   = 4 << 20
	archiveMaxDecompressed = 16 << 20
	archiveMaxEntries      = 2000
	archiveMaxEntrySize    = 4 << 20
	archiveMaxKeyFile      = 64 * 1024

	dropbearEd25519HostKey = "etc/dropbear/dropbear_ed25519_host_key"
	authorizedKeysPath     = "etc/dropbear/authorized_keys"
)

var errBadArchive = errors.New("the device did not return a valid configuration archive")

type archiveInfo struct {
	Files int
	// ContentDigest covers entry names, sizes and contents in archive order but not
	// timestamps, so two backups of an unchanged device match.
	ContentDigest string
	// HostKeyFingerprint is the SHA256 fingerprint of the archived dropbear ed25519
	// host key, or "" when the archive has none.
	HostKeyFingerprint string
	// AuthorizedKeys is the archived authorized_keys file, nil when absent or oversized.
	AuthorizedKeys []byte
}

func inspectBackupArchive(data []byte) (archiveInfo, error) {
	var info archiveInfo
	if len(data) == 0 || len(data) > archiveMaxCompressed {
		return info, errBadArchive
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return info, errBadArchive
	}
	defer zr.Close()
	// One shared budget bounds total decompressed bytes, headers included.
	budget := &io.LimitedReader{R: zr, N: archiveMaxDecompressed + 1}
	tr := tar.NewReader(budget)
	digest := sha256.New()
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || budget.N <= 0 {
			return info, errBadArchive
		}
		if info.Files++; info.Files > archiveMaxEntries {
			return info, errBadArchive
		}
		name, ok := cleanArchiveName(hdr.Name)
		if !ok {
			return info, errBadArchive
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default: // symlinks, hardlinks, devices, fifos
			return info, errBadArchive
		}
		if hdr.Size < 0 || hdr.Size > archiveMaxEntrySize {
			return info, errBadArchive
		}
		content, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
		if err != nil || int64(len(content)) != hdr.Size || budget.N <= 0 {
			return info, errBadArchive
		}
		fmt.Fprintf(digest, "%s\x00%d\x00", name, len(content))
		digest.Write(content)
		switch {
		case name == dropbearEd25519HostKey:
			info.HostKeyFingerprint = dropbearEd25519Fingerprint(content)
		case name == authorizedKeysPath && len(content) <= archiveMaxKeyFile:
			info.AuthorizedKeys = content
		}
	}
	if info.Files == 0 {
		return info, errBadArchive
	}
	info.ContentDigest = fmt.Sprintf("%x", digest.Sum(nil))
	return info, nil
}

// cleanArchiveName accepts only relative paths that stay inside the archive root.
// sysupgrade writes names like "etc/config/network" (some tars add a "./" prefix).
func cleanArchiveName(name string) (string, bool) {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) || len(name) > 512 {
		return "", false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", false
		}
	}
	clean := path.Clean(name)
	if clean == "." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// dropbearEd25519Fingerprint reads dropbear's ed25519 key file: an SSH-style string
// "ssh-ed25519" followed by one string holding the 32-byte private seed and then the
// 32-byte public key. It returns the public key's SHA256 fingerprint, which is what
// SSH clients and OpenManage pin, and never uses the private half.
func dropbearEd25519Fingerprint(content []byte) string {
	fields := make([][]byte, 0, 2)
	rest := content
	for len(fields) < 2 && len(rest) >= 4 {
		n := binary.BigEndian.Uint32(rest)
		rest = rest[4:]
		if uint64(n) > uint64(len(rest)) {
			return ""
		}
		fields = append(fields, rest[:n])
		rest = rest[n:]
	}
	if len(fields) < 2 || string(fields[0]) != "ssh-ed25519" || len(fields[1]) != 2*ed25519.PublicKeySize {
		return ""
	}
	pub, err := ssh.NewPublicKey(ed25519.PublicKey(fields[1][ed25519.PublicKeySize:]))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(pub)
}
