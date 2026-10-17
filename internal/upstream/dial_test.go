package upstream

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDNS answers A queries for any name with 127.0.0.1 once it has refused failFirst
// queries with SERVFAIL. AAAA queries always get an IPv6 answer, like the bad cache.
func fakeDNS(t *testing.T, failFirst int32) (*net.Resolver, *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var aQueries atomic.Int32
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			end := 12
			for q[end] != 0 {
				end += int(q[end]) + 1
			}
			end += 5 // root label, qtype, qclass
			qtype := binary.BigEndian.Uint16(q[end-4:])
			resp := append([]byte{}, q[:end]...)
			resp[2] |= 0x80 // QR
			resp[3] = 0x80  // RA, rcode 0
			binary.BigEndian.PutUint16(resp[6:], 0)
			if qtype == 1 && aQueries.Add(1) <= failFirst {
				resp[3] |= 2 // SERVFAIL
			} else {
				rdata := []byte{127, 0, 0, 1}
				if qtype == 28 {
					rdata = net.ParseIP("2001:db8::1").To16()
				}
				binary.BigEndian.PutUint16(resp[6:], 1)
				resp = append(resp, 0xc0, 0x0c)
				resp = binary.BigEndian.AppendUint16(resp, qtype)
				resp = append(resp, 0, 1, 0, 0, 0, 60)
				resp = binary.BigEndian.AppendUint16(resp, uint16(len(rdata)))
				resp = append(resp, rdata...)
			}
			pc.WriteTo(resp, from)
		}
	}()
	addr := pc.LocalAddr().String()
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp4", addr)
	}}
	return r, &aQueries
}

// brokenResolver fails every lookup, standing in for a wedged system resolver.
var brokenResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("resolver unavailable")
}}

func listen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func TestDialerFallsBackAndRetriesUntilIPv4Resolves(t *testing.T) {
	direct, aQueries := fakeDNS(t, 3)
	d := &Dialer{System: brokenResolver, Direct: direct, Backoff: time.Millisecond}
	c, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("chatgpt.example", listen(t)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close()
	if got := c.RemoteAddr().(*net.TCPAddr).IP.To4(); got == nil {
		t.Fatalf("dialed %v, want IPv4", c.RemoteAddr())
	}
	if aQueries.Load() < 4 {
		t.Fatalf("A queries = %d, want the 3 failures retried", aQueries.Load())
	}
}

func TestDialerGivesUpAfterAttempts(t *testing.T) {
	direct, aQueries := fakeDNS(t, 1000)
	d := &Dialer{System: brokenResolver, Direct: direct, Attempts: 3, Backoff: time.Millisecond}
	if _, err := d.DialContext(context.Background(), "tcp", "chatgpt.example:443"); err == nil {
		t.Fatal("dial succeeded with every lookup failing")
	}
	if aQueries.Load() == 0 {
		t.Fatal("direct resolver never asked")
	}
}

func TestDialerStopsRetryingWhenContextEnds(t *testing.T) {
	d := &Dialer{System: brokenResolver, Direct: brokenResolver, Attempts: 100, Backoff: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := d.DialContext(ctx, "tcp", "chatgpt.example:443"); err == nil {
		t.Fatal("dial succeeded")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("retry loop ignored the context deadline")
	}
}
