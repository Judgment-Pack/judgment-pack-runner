// jpack-source-worker is a long-lived caller of a configured Gateway. It owns
// no signing key. Runner processes reconnect through its private owner token.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "source worker:", e)
		os.Exit(1)
	}
}
func run() error {
	dir := flag.String("store", "", "absolute private caller-state directory (outside the Gateway store)")
	gateway := flag.String("gateway", "", "persistent Gateway origin")
	token := flag.String("gateway-token-file", "", "optional private Gateway bearer-token file")
	port := flag.Int("port", 8792, "loopback port (use a stable port in the installed Runner connection)")
	flag.Parse()
	worker, e := runner.OpenSourceWorker(runner.SourceWorkerConfig{Dir: *dir, Gateway: *gateway, GatewayTokenFile: *token})
	if e != nil {
		return e
	}
	defer worker.Close()
	listener, e := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", *port))
	if e != nil {
		return e
	}
	server := &http.Server{Handler: worker.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	defer server.Close()
	if e = json.NewEncoder(os.Stdout).Encode(map[string]string{"url": "http://" + listener.Addr().String(), "tokenFile": worker.TokenFile(), "protocol": "source-worker/1"}); e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errors := make(chan error, 1)
	go func() { errors <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		return nil
	case e = <-errors:
		if e == http.ErrServerClosed {
			return nil
		}
		return e
	}
}
