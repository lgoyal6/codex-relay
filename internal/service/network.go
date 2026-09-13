package service

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// UpstreamProxySetting holds an outbound proxy URL, or is empty for a direct connection.
const UpstreamProxySetting = "upstream_proxy"

// proxyCacheTTL bounds how stale the resolved proxy can be.
//
// The proxy function is called on every upstream request, and reading a setting from SQLite
// on that path would put a database round trip in front of every turn. A few seconds of
// staleness after changing the setting is invisible; a per-turn query would not be.
const proxyCacheTTL = 5 * time.Second

type proxyCache struct {
	mu       sync.Mutex
	at       time.Time
	resolved *url.URL
}

// UpstreamProxyFor resolves the configured outbound proxy for a request.
//
// Returns nil with no error when nothing is configured, which tells the transport to fall
// back to the environment and then to a direct connection.
func (s *Service) UpstreamProxyFor(*http.Request) (*url.URL, error) {
	s.proxyCache.mu.Lock()
	defer s.proxyCache.mu.Unlock()
	if !s.proxyCache.at.IsZero() && s.Clock.Now().Sub(s.proxyCache.at) < proxyCacheTTL {
		return s.proxyCache.resolved, nil
	}
	raw, _ := s.GetSetting(context.Background(), UpstreamProxySetting)
	var resolved *url.URL
	if raw = strings.TrimSpace(raw); raw != "" {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			resolved = u
		}
	}
	s.proxyCache.at, s.proxyCache.resolved = s.Clock.Now(), resolved
	return resolved, nil
}

// SetUpstreamProxy validates before storing. A proxy that cannot be parsed would silently
// mean "direct", and the user would be told their traffic is proxied when it is not.
func (s *Service) SetUpstreamProxy(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("that is not a usable proxy URL: %w", err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("proxy scheme %q is not supported; use http, https or socks5", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("a proxy URL needs a host, for example http://127.0.0.1:8080")
		}
	}
	if err := s.SetSetting(ctx, UpstreamProxySetting, raw); err != nil {
		return err
	}
	// Drop the cache so the change takes effect on the next request rather than in a few
	// seconds, which would make a "save then test" flow report the old value.
	s.proxyCache.mu.Lock()
	s.proxyCache.at = time.Time{}
	s.proxyCache.mu.Unlock()
	return nil
}

// UpstreamProxyValue is what is currently configured, for display.
func (s *Service) UpstreamProxyValue(ctx context.Context) string {
	v, _ := s.GetSetting(ctx, UpstreamProxySetting)
	return v
}
