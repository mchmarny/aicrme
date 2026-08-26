package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"runtime"
)

// newLaunchToken returns the secret that authenticates one browser to this
// process. 256 bits from crypto/rand, URL-safe because it travels in a query
// string, and unguessable because any local process can reach loopback.
func newLaunchToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating the launch token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// requireLoopback refuses an address that is reachable from off this machine.
//
// Refused, not warned about. The console drives a cluster with the operator's
// own credentials and authenticates with a token printed to their terminal;
// on a shared or office network, binding it to 0.0.0.0 hands that to anyone
// who can reach the port. A warning is the wrong instrument for a mistake
// whose blast radius is somebody else's cluster.
//
// An empty host -- what ":8080" means to net.Listen -- is every interface,
// which is exactly the case this exists to stop.
func requireLoopback(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--addr %q is not host:port: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("--addr %q binds every interface; use 127.0.0.1 or localhost", addr)
	}

	// A literal is answered directly; a name is resolved, because "localhost"
	// is the spelling most operators reach for and it is legitimate.
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("--addr %q is not a loopback address; this console must not be reachable off this machine", addr)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("--addr %q: resolving %s: %w", addr, host, err)
	}
	for _, addrInfo := range ips {
		if ip := addrInfo.IP; !ip.IsLoopback() {
			return fmt.Errorf("--addr %q resolves to %s, which is not loopback; this console must not be reachable off this machine",
				addr, ip)
		}
	}
	return nil
}

// consoleURL is what the operator opens: the bound address plus the launch
// token as a query parameter. The SPA exchanges it for a cookie and strips it
// from the address bar immediately (web/src/App.tsx).
func consoleURL(addr net.Addr, token string) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		// An addr that does not split is not something to fail a start over;
		// use it verbatim and let the operator see what it was.
		return "http://" + addr.String() + "/?t=" + url.QueryEscape(token)
	}
	return "http://" + net.JoinHostPort(host, port) + "/?t=" + url.QueryEscape(token)
}

// announce prints the tokenized URL and, if asked, opens a browser at it.
//
// stdout unconditionally, whether or not --open was passed and whether or not
// the open succeeded. The logs go to stderr precisely so this line can be the
// one thing stdout carries: an operator on a headless box, over SSH, or in CI
// has no browser to open and the URL is the only way in. A failed open that
// printed nothing would be a dead end with the secret trapped inside the
// process.
func announce(ctx context.Context, consoleURL string, open bool) {
	fmt.Fprintln(os.Stdout, consoleURL)
	if !open {
		return
	}
	if err := openBrowser(ctx, consoleURL); err != nil {
		slog.Warn("could not open a browser; open the URL above by hand", "error", err)
	}
}

// openBrowser hands the URL to the platform's opener. Failure is not fatal
// anywhere: announce has already printed the URL.
//
// The context is detached from the caller's with context.WithoutCancel. These
// helpers hand off to a browser and exit, but on a desktop where the opener
// execs the browser directly, a cancelable context would make shutting the
// console down close the operator's browser -- which is not this program's to
// close.
func openBrowser(ctx context.Context, target string) error {
	ctx = context.WithoutCancel(ctx)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", target)
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", target)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", target)
	default:
		return fmt.Errorf("no browser opener known for %s", runtime.GOOS)
	}
	return cmd.Start()
}
