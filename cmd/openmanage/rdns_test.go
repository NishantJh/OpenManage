package main

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestReverseDNS(t *testing.T) {
	calls := 0
	r := &reverseDNS{lookup: func(_ context.Context, ip string) ([]string, error) {
		calls++
		switch ip {
		case "192.168.1.50":
			return []string{"Nishants-iPhone.lan."}, nil
		case "192.168.1.51":
			return []string{"<bad>."}, nil
		}
		return nil, errors.New("no such host")
	}}
	ctx := context.Background()
	if got := r.name(ctx, "192.168.1.50"); got != "Nishants-iPhone" {
		t.Fatalf("name = %q", got)
	}
	if got := r.name(ctx, "192.168.1.51"); got != "" {
		t.Fatalf("an unsafe PTR must be dropped, got %q", got)
	}
	if got := r.name(ctx, "192.168.1.52"); got != "" {
		t.Fatalf("a failed lookup has no name, got %q", got)
	}
	before := calls
	r.name(ctx, "192.168.1.50")
	r.name(ctx, "192.168.1.52")
	if calls != before {
		t.Fatal("answers, including misses, must be cached")
	}
	for _, ip := range []string{"8.8.8.8", "fe80::1", "not-an-ip", ""} {
		if got := r.name(ctx, ip); got != "" || calls != before {
			t.Fatalf("%q must not be looked up", ip)
		}
	}
}

func TestResolverForOnlyUsesPrivateIPv4(t *testing.T) {
	for _, bad := range []string{"", "dns.example.com", "8.8.8.8", "::1", "fd00::1"} {
		if resolverFor(bad) != net.DefaultResolver {
			t.Fatalf("%q must fall back to the default resolver", bad)
		}
	}
	if r := resolverFor("192.168.1.1"); r == net.DefaultResolver || r.Dial == nil {
		t.Fatal("a private IPv4 DNS server must get its own resolver")
	}
	if got := hostFromPTR("phone.internal.lan."); got != "phone" {
		t.Fatalf("internal domain not stripped: %q", got)
	}
}
