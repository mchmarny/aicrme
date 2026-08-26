package console

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestLaunchTokenIsLongAndUnguessable(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		token, err := newLaunchToken()
		if err != nil {
			t.Fatalf("newLaunchToken() error = %v", err)
		}
		// 32 bytes, base64url with no padding.
		if len(token) != 43 {
			t.Fatalf("token %q is %d characters, want 43 (256 bits)", token, len(token))
		}
		if seen[token] {
			t.Fatalf("newLaunchToken() returned %q twice in 100 calls", token)
		}
		seen[token] = true
	}
}

// The token travels in a query string. A padded or standard-alphabet encoding
// would put '+' and '=' in the URL, which survives escaping but makes the
// value the operator copies out of a terminal fragile.
func TestLaunchTokenIsURLSafe(t *testing.T) {
	token, err := newLaunchToken()
	if err != nil {
		t.Fatalf("newLaunchToken() error = %v", err)
	}
	if strings.ContainsAny(token, "+/=") {
		t.Errorf("token %q contains characters that need escaping in a URL", token)
	}
}

func TestNonLoopbackAddressesAreRefused(t *testing.T) {
	for _, addr := range []string{
		":8080",          // every interface
		"0.0.0.0:8080",   // every interface, spelled out
		"192.168.1.5:80", // a LAN address
		"[::]:8080",      // every interface, v6
	} {
		t.Run(addr, func(t *testing.T) {
			if err := requireLoopback(context.Background(), addr); err == nil {
				t.Errorf("requireLoopback(%q) = nil; the console would be reachable off this machine", addr)
			}
		})
	}
}

func TestLoopbackAddressesAreAccepted(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		t.Run(addr, func(t *testing.T) {
			if err := requireLoopback(context.Background(), addr); err != nil {
				t.Errorf("requireLoopback(%q) error = %v", addr, err)
			}
		})
	}
}

func TestMalformedAddressIsRefusedByName(t *testing.T) {
	if err := requireLoopback(context.Background(), "127.0.0.1"); err == nil {
		t.Error("requireLoopback() accepted an address with no port")
	} else if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error = %v, want it to name the address given", err)
	}
}

func TestConsoleURLCarriesTheTokenAndTheBoundPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = ln.Close() }()

	got := consoleURL(ln.Addr(), "tok-en")
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}
	want := "http://127.0.0.1:" + port + "/?t=tok-en"
	if got != want {
		t.Errorf("consoleURL() = %q, want %q", got, want)
	}
}

// Port 0 is the default, so the URL the operator is handed is the only place
// the real port ever appears.
func TestConsoleURLReportsTheOSAssignedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = ln.Close() }()

	if strings.Contains(consoleURL(ln.Addr(), "t"), ":0/") {
		t.Error("consoleURL() reported port 0 rather than the port the OS assigned")
	}
}

func TestConsoleURLBracketsAnIPv6Host(t *testing.T) {
	addr := &net.TCPAddr{IP: net.IPv6loopback, Port: 9999}
	if got, want := consoleURL(addr, "t"), "http://[::1]:9999/?t=t"; got != want {
		t.Errorf("consoleURL() = %q, want %q", got, want)
	}
}
