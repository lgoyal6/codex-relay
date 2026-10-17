package upstream

import (
	"context"
	"errors"
	"net"
	"time"
)

// Dialer reaches the ChatGPT backend over IPv4. After a wake or a network change the
// macOS resolver can hand back only AAAA records (unroutable on an IPv4-only network) or
// nothing at all for a few minutes, while the DNS servers themselves answer fine. So it
// resolves A records only, asks the servers directly when the system resolver fails, and
// retries for a while before giving up.
type Dialer struct {
	Dialer   net.Dialer
	System   *net.Resolver // the OS resolver; nil means net.DefaultResolver
	Direct   *net.Resolver // queries resolv.conf servers itself; nil means a PreferGo resolver
	Attempts int           // lookup rounds before giving up; 0 means 5
	Backoff  time.Duration // wait after the first failed round, doubling; 0 means 250ms
}

// NewDialer returns the dialer every upstream client uses.
func NewDialer() *Dialer {
	return &Dialer{Dialer: net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}}
}

func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) != nil {
		return d.Dialer.DialContext(ctx, network, addr)
	}
	ips, err := d.lookup4(ctx, host)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, ip := range ips {
		c, err := d.Dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

func (d *Dialer) lookup4(ctx context.Context, host string) ([]net.IP, error) {
	system, direct := d.System, d.Direct
	if system == nil {
		system = net.DefaultResolver
	}
	if direct == nil {
		direct = &net.Resolver{PreferGo: true}
	}
	attempts, wait := d.Attempts, d.Backoff
	if attempts <= 0 {
		attempts = 5
	}
	if wait <= 0 {
		wait = 250 * time.Millisecond
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, errors.Join(ctx.Err(), lastErr)
			case <-t.C:
			}
			wait *= 2
		}
		for _, r := range []*net.Resolver{system, direct} {
			ips, err := r.LookupIP(ctx, "ip4", host)
			if err == nil && len(ips) > 0 {
				return ips, nil
			}
			if err == nil {
				err = &net.DNSError{Err: "no IPv4 address", Name: host, IsNotFound: true}
			}
			lastErr = err
		}
	}
	return nil, lastErr
}
