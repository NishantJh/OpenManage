package main

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	rdnsTimeout    = time.Second
	rdnsHitTTL     = time.Hour
	rdnsMissTTL    = 5 * time.Minute
	rdnsMaxPerPoll = 16
	rdnsPollBudget = 8 * time.Second
	rdnsCacheLimit = 2048
)

// reverseDNS names a client from the PTR record of its IP, for networks where the DHCP
// host is not enrolled but its DNS publishes lease names. Only private IPv4 addresses are
// looked up, each with a short timeout, and answers are cached in memory.
type reverseDNS struct {
	mu     sync.Mutex
	cache  map[string]rdnsEntry
	lookup func(ctx context.Context, ip string) ([]string, error) // net.DefaultResolver.LookupAddr unless set
}

type rdnsEntry struct {
	name string
	till time.Time
}

// hostFromPTR keeps the first label of a PTR answer if it is a plain hostname.
func hostFromPTR(ptr string) string {
	host, _, _ := strings.Cut(strings.TrimSuffix(strings.TrimSpace(ptr), "."), ".")
	if !leaseNamePattern.MatchString(host) {
		return ""
	}
	return host
}

func (r *reverseDNS) name(ctx context.Context, ip string) string {
	if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil || !parsed.IsPrivate() {
		return ""
	}
	now := time.Now()
	r.mu.Lock()
	if e, ok := r.cache[ip]; ok && now.Before(e.till) {
		r.mu.Unlock()
		return e.name
	}
	r.mu.Unlock()
	lookup := r.lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupAddr
	}
	lctx, cancel := context.WithTimeout(ctx, rdnsTimeout)
	defer cancel()
	name := ""
	if ptrs, err := lookup(lctx, ip); err == nil {
		for _, ptr := range ptrs {
			if name = hostFromPTR(ptr); name != "" {
				break
			}
		}
	}
	ttl := rdnsMissTTL
	if name != "" {
		ttl = rdnsHitTTL
	}
	r.mu.Lock()
	if r.cache == nil || len(r.cache) >= rdnsCacheLimit {
		r.cache = map[string]rdnsEntry{}
	}
	r.cache[ip] = rdnsEntry{name: name, till: now.Add(ttl)}
	r.mu.Unlock()
	return name
}
