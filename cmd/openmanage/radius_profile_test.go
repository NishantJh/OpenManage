package main

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
)

func TestValidSSID(t *testing.T) {
	for ssid, want := range map[string]bool{"Home": true, "Café <5G> & co": true, "": false,
		strings.Repeat("a", 33): false, "bad\nname": false, "\xff": false} {
		if got := validSSID(ssid); got != want {
			t.Errorf("validSSID(%q) = %v", ssid, got)
		}
	}
}

func TestAppleWiFiProfile(t *testing.T) {
	caDER, _, err := newRADIUSCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	profile := appleWiFiProfile(caDER, `Home <"5G"> & co`)
	// Well-formed XML, with the SSID escaped and the CA embedded.
	dec := xml.NewDecoder(bytes.NewReader(profile))
	dec.Strict = true
	var texts []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("profile is not well-formed XML: %v", err)
		}
		if cd, ok := tok.(xml.CharData); ok {
			texts = append(texts, string(cd))
		}
	}
	all := strings.Join(texts, "\n")
	for _, want := range []string{`Home <"5G"> & co`, base64.StdEncoding.EncodeToString(caDER), radiusServerName, "PAP", "com.apple.wifi.managed"} {
		if !strings.Contains(all, want) {
			t.Errorf("profile is missing %q", want)
		}
	}
	if bytes.Contains(profile, []byte("<5G>")) {
		t.Fatal("SSID was not escaped")
	}
	// The same CA and SSID give the same identifiers, so reinstalling replaces.
	if !bytes.Equal(profile, appleWiFiProfile(caDER, `Home <"5G"> & co`)) {
		t.Fatal("profile identifiers are not stable")
	}
}
