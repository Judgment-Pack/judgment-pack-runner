// jpack-runner is a local, single-owner companion. Boot authority is delivered
// over stdin, never project files, URLs, arguments, or browser-provided config.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/buildinfo"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"
)

func main() {
	err := error(nil)
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("jpack-runner", buildinfo.Version())
	} else if len(os.Args) > 1 && os.Args[1] == "verify-run" {
		err = verifyRun(os.Args[2:], os.Stdout, os.Stderr)
	} else {
		err = serve()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		os.Exit(1)
	}
}
func serve() error {
	input := bufio.NewReaderSize(os.Stdin, 128<<10)
	line, err := input.ReadSlice('\n')
	if err != nil || len(line) > 128<<10 {
		return fmt.Errorf("bounded boot configuration required")
	}
	var boot struct {
		Dir                string                     `json:"dir"`
		Runtime            string                     `json:"runtime"`
		Workspace          string                     `json:"workspace"`
		Owner              string                     `json:"owner"`
		Token              string                     `json:"token"`
		CloudConnections   []runner.CloudConnection   `json:"cloudConnections,omitempty"`
		GatewayConnections []runner.GatewayConnection `json:"gatewayConnections,omitempty"`
		InputRoot          string                     `json:"inputRoot,omitempty"`
		InputProfiles      []runner.InputProfile      `json:"inputProfiles,omitempty"`
	}
	if json.Unmarshal([]byte(line), &boot) != nil || len(boot.Token) < 32 {
		return fmt.Errorf("invalid boot configuration")
	}
	service, err := runner.Open(runner.Config{Dir: boot.Dir, Runtime: boot.Runtime, Workspace: boot.Workspace, Owner: boot.Owner, InputProfiles: boot.InputProfiles, InputRoot: boot.InputRoot, CloudConnections: boot.CloudConnections, GatewayConnections: boot.GatewayConnections})
	if err != nil {
		return err
	}
	defer service.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	// A release check can run validation, lock, rehearsal and saved tests, each
	// with a 30-second subprocess deadline. Allow the bounded sequence to finish.
	server := &http.Server{Handler: service.Handler(boot.Token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 40 * time.Second, WriteTimeout: 135 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	defer server.Close()
	if err = json.NewEncoder(os.Stdout).Encode(map[string]string{"url": "http://" + listener.Addr().String(), "protocol": "jobs/1"}); err != nil {
		return err
	}
	closed := make(chan struct{})
	go func() { io.Copy(io.Discard, input); close(closed) }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener) }()
	select {
	case <-closed:
	case <-ctx.Done():
	case err = <-failed:
		if err != http.ErrServerClosed {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	server.Shutdown(shutdown)
	return nil
}

func verifyRun(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify-run", flag.ContinueOnError)
	file := flags.String("file", "", "retained verification export")
	profilesPath := flags.String("profiles", "", "independently trusted input profiles JSON")
	release := flags.String("release-digest", "", "independently trusted frozen release digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *file == "" || *profilesPath == "" || *release == "" || flags.NArg() != 0 {
		return fmt.Errorf("file, profiles and release-digest are required")
	}
	read := func(path string) ([]byte, error) {
		f, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, 8<<20+1))
		if len(b) > 8<<20 {
			return nil, fmt.Errorf("verification input exceeds limit")
		}
		return b, e
	}
	raw, err := read(*file)
	if err != nil {
		return err
	}
	p, err := read(*profilesPath)
	if err != nil {
		return err
	}
	profiles, err := runner.ParseInputProfiles(p)
	if err != nil {
		return err
	}
	if err = runner.VerifyRun(raw, profiles, *release); err != nil {
		return err
	}
	// verified-inputs covers the inputs, not the result. Nothing here compares the
	// retained disposition with an evaluation, and both outputs say so.
	fmt.Fprintln(stderr, "verified-inputs: the run's inputs match its record. Its disposition was not checked: this does not say the run decided what its record says.")
	return json.NewEncoder(stdout).Encode(map[string]string{"status": "verified-inputs", "retainedDisposition": "not-checked", "scope": "retained input derivation and audit binding; not sealed-session completeness or policy truth"})
}
