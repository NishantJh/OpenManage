package main

import (
	"net"
	"strings"
	"sync"
	"time"
)

const (
	loginFailureLimit = 10
	loginFailureWindow = 5 * time.Minute
	loginBlockDuration = 5 * time.Minute
	loginLimiterMaxEntries = 4096
)

type loginAttempt struct {
	failures    int
	windowStart time.Time
	blockedUntil time.Time
	lastSeen    time.Time
}

// loginRateLimiter bounds memory usage and tracks failed logins by the network
// peer address. It deliberately does not trust forwarded headers; deployments
// behind a reverse proxy should rate-limit at that proxy as well.
type loginRateLimiter struct {
	mu          sync.Mutex
	entries     map[string]loginAttempt
	maxFailures int
	window      time.Duration
	block       time.Duration
	maxEntries  int
}

func newLoginRateLimiter(maxFailures int, window, block time.Duration, maxEntries int) *loginRateLimiter {
	if maxFailures < 1 {
		maxFailures = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	if block <= 0 {
		block = window
	}
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &loginRateLimiter{
		entries: make(map[string]loginAttempt), maxFailures: maxFailures,
		window: window, block: block, maxEntries: maxEntries,
	}
}

func (l *loginRateLimiter) isBlocked(client string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	entry, ok := l.entries[client]
	if !ok {
		return false, 0
	}
	entry.lastSeen = now
	l.entries[client] = entry
	if entry.blockedUntil.After(now) {
		return true, entry.blockedUntil.Sub(now)
	}
	return false, 0
}

func (l *loginRateLimiter) recordFailure(client string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	entry, ok := l.entries[client]
	if !ok {
		if len(l.entries) >= l.maxEntries {
			l.evictOldest()
		}
		entry = loginAttempt{windowStart: now}
	}
	if !entry.blockedUntil.IsZero() && !entry.blockedUntil.After(now) {
		entry = loginAttempt{windowStart: now}
	}
	if now.Sub(entry.windowStart) >= l.window || now.Before(entry.windowStart) {
		entry.failures = 0
		entry.windowStart = now
		entry.blockedUntil = time.Time{}
	}
	entry.failures++
	entry.lastSeen = now
	if entry.failures >= l.maxFailures {
		entry.blockedUntil = now.Add(l.block)
	}
	l.entries[client] = entry
}

func (l *loginRateLimiter) clear(client string) {
	l.mu.Lock()
	delete(l.entries, client)
	l.mu.Unlock()
}

func (l *loginRateLimiter) prune(now time.Time) {
	for client, entry := range l.entries {
		if !entry.blockedUntil.After(now) && now.Sub(entry.lastSeen) > l.window {
			delete(l.entries, client)
		}
	}
}

func (l *loginRateLimiter) evictOldest() {
	var oldestClient string
	var oldest time.Time
	for client, entry := range l.entries {
		if oldestClient == "" || entry.lastSeen.Before(oldest) {
			oldestClient, oldest = client, entry.lastSeen
		}
	}
	if oldestClient != "" {
		delete(l.entries, oldestClient)
	}
}

func loginClientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.Trim(strings.TrimSpace(remoteAddr), "[]")
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return "unknown"
}

var loginAttempts = newLoginRateLimiter(
	loginFailureLimit, loginFailureWindow, loginBlockDuration, loginLimiterMaxEntries,
)
