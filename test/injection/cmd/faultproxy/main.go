// Command faultproxy runs the controllable HTTP fault proxy in front of
// cara-server, so an E2E shell script can decide when an agent-to-server call
// fails.
//
// It prints the two addresses it listened on and then serves until signalled:
//
//	listen=http://127.0.0.1:38411
//	control=http://127.0.0.1:38412
//
// Point the agent at the first and drive the second, for example:
//
//	faultproxy -upstream "$server_url" -addr-file "$test_root/proxy.addr" &
//	# ... read listen= and control= out of proxy.addr ...
//	curl -fsS -X POST "$control/control/rules" -d '{
//	  "id":"ownership","method":"GET","path_prefix":"/api/v1/projects",
//	  "query_contains":["nodeRef=node-a"],"query_absent":["phase="],
//	  "hold":true,"respond":"status","status":503,"times":1}'
//	curl -fsS "$control/control/rules/ownership/wait?timeout=30s"   # blocks
//	# ... build the state the scenario needs ...
//	curl -fsS -X POST "$control/control/rules/ownership/release"
//
// The wait is the point: it returns only once the request is parked inside the
// proxy, so the state the scenario builds next is in place before the failure
// lands. A wait that times out answers 408, which a script must treat as a
// failed injection rather than a passing test.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"NYCU-SDC/caravanserai/test/injection/httpfault"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "faultproxy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	upstream := flag.String("upstream", "", "server to forward to, e.g. http://127.0.0.1:8080 (required)")
	listenAddr := flag.String("listen", "127.0.0.1:0", "address to serve the proxy on; port 0 picks a free one")
	controlAddr := flag.String("control", "127.0.0.1:0", "address to serve the control API on")
	addrFile := flag.String("addr-file", "", "write the chosen addresses here, for a script to read")
	flag.Parse()

	if *upstream == "" {
		return errors.New("-upstream is required")
	}
	target, err := url.Parse(*upstream)
	if err != nil {
		return fmt.Errorf("parse -upstream: %w", err)
	}
	if target.Scheme == "" || target.Host == "" {
		return fmt.Errorf("-upstream %q needs a scheme and a host", *upstream)
	}

	proxy := httpfault.New(target)

	// Both listeners open before anything is announced, so a script that reads
	// the address file can connect immediately.
	proxyListener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listenAddr, err)
	}
	defer func() { _ = proxyListener.Close() }()

	controlListener, err := net.Listen("tcp", *controlAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *controlAddr, err)
	}
	defer func() { _ = controlListener.Close() }()

	proxyURL := "http://" + proxyListener.Addr().String()
	controlURL := "http://" + controlListener.Addr().String()

	if *addrFile != "" {
		if err := writeAddrFile(*addrFile, proxyURL, controlURL); err != nil {
			return err
		}
	}
	fmt.Printf("listen=%s\ncontrol=%s\nupstream=%s\n", proxyURL, controlURL, target)
	_ = os.Stdout.Sync()

	// A held request must be able to outlive any write deadline, so the proxy
	// server sets none. The caller's own timeout is what bounds a hold, and
	// the proxy records that as client_gone.
	proxyServer := &http.Server{Handler: proxy.Handler()}
	controlServer := &http.Server{Handler: proxy.ControlHandler()}

	errs := make(chan error, 2)
	go func() { errs <- serve(proxyServer, proxyListener, "proxy") }()
	go func() { errs <- serve(controlServer, controlListener, "control") }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// Release anything still parked before shutting down, so a scenario that
	// is torn down mid-injection does not leave the agent blocked on a socket
	// that will never answer.
	proxy.Reset()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = proxyServer.Shutdown(shutdownCtx)
	_ = controlServer.Shutdown(shutdownCtx)
	return nil
}

func serve(server *http.Server, listener net.Listener, name string) error {
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s server: %w", name, err)
	}
	return nil
}

// writeAddrFile writes the addresses atomically, so a script polling for the
// file never reads it half-written.
func writeAddrFile(path, proxyURL, controlURL string) error {
	body := fmt.Sprintf("listen=%s\ncontrol=%s\n", proxyURL, controlURL)
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".faultproxy-addr-*")
	if err != nil {
		return fmt.Errorf("create temp addr file in %s: %w", dir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write addr file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close addr file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("install addr file at %s: %w", path, err)
	}
	return nil
}
