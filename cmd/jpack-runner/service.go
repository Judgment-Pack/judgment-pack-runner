package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// Service mode (runner #214 and #236, docs/design/service-mode.md).
//
// `jpack-runner serve --config <path>` runs Runner as a long-running service
// rather than a host's companion. Its configuration is one file, held to the
// rules the signing key is held to (runner.ReadServiceConfig): the boot
// line's members, which it reads as the boot line is read, and listen, the
// address the operator chose. Its lifetime is the process's: it reads no
// stdin, and stops on an interrupt or a termination signal. It listens on a
// loopback address unless the configuration has an identity, by which every
// request but GET /v1/health holds an identity token, and
// acceptOtherHostsOverPlainHTTP true: Runner terminates no TLS. A companion,
// started with no argument, is as it was.

// serviceMembers are the members a service configuration holds beside the
// boot line's.
var serviceMembers = []string{"listen", "identity", "acceptOtherHostsOverPlainHTTP"}

// companionMembers are boot members a service configuration does not take:
// both bind Runner to the host that starts it (docs/design/
// executor-ownership.md), and in service mode no host starts it.
var companionMembers = []string{"executorOwnership", "executorAdmission"}

// bootMemberNames are the boot line's members: bootLine's, and the three it
// reads on their own.
func bootMemberNames() []string {
	names := []string{"requiredCapabilities", "executorOwnership", "executorAdmission"}
	t := reflect.TypeOf(bootLine{})
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		names = append(names, name)
	}
	return names
}

// serviceConfig is a service configuration as read.
type serviceConfig struct {
	runner   runner.Config
	token    string
	identity *runner.IdentityConfig
	listen   netip.AddrPort
}

// serveCommand is `jpack-runner serve --config <path>`.
func serveCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) != 2 || args[0] != "--config" {
		return errors.New("serve takes --config and the absolute path of its service configuration, and nothing else")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveConfigured(ctx, args[1], stdout, stderr)
}

// readServiceConfig reads and holds a service configuration, before anything
// is opened: the file itself (runner.ReadServiceConfig); one JSON object, each
// member once and spelled exactly, every member one Runner reads; the boot
// line's members, read by the boot line's rules, but for executorOwnership
// and executorAdmission, which it refuses; token, the owner bearer, which a
// configuration with identity does not take and one without needs; listen;
// and acceptOtherHostsOverPlainHTTP, true or false.
func readServiceConfig(path string) (serviceConfig, error) {
	raw, err := runner.ReadServiceConfig(path)
	if err != nil {
		return serviceConfig{}, fmt.Errorf("the service configuration is refused: %w", err)
	}
	members, err := serviceObject(raw)
	if err != nil {
		return serviceConfig{}, err
	}
	known := append(bootMemberNames(), serviceMembers...)
	for name := range members {
		if slices.Contains(known, name) {
			continue
		}
		for _, k := range known {
			if strings.EqualFold(name, k) {
				return serviceConfig{}, fmt.Errorf("the service configuration's member %q is %s spelled another way: member names are read exactly", name, k)
			}
		}
		return serviceConfig{}, fmt.Errorf("the service configuration has a member Runner does not read, %q", name)
	}
	for _, name := range companionMembers {
		if _, present := members[name]; present {
			return serviceConfig{}, fmt.Errorf("%s binds Runner to the host that starts it as its companion, and a service configuration does not take it", name)
		}
	}
	var c serviceConfig
	if v, present := members["identity"]; present {
		identity, err := runner.ParseIdentityConfig(v)
		if err != nil {
			return serviceConfig{}, err
		}
		c.identity = &identity
	}
	token, present := members["token"]
	switch {
	case c.identity != nil && present:
		return serviceConfig{}, errors.New("token is the owner bearer, which a configuration with identity does not take: every request on its control API holds an identity token instead")
	case c.identity == nil:
		var bearer string
		if !present || json.Unmarshal(token, &bearer) != nil || len(bearer) < 32 {
			return serviceConfig{}, errors.New("token must be the owner bearer, at least 32 characters: without identity, every request on the control API holds it")
		}
	}
	if c.runner, c.token, err = bootMembers(raw, c.identity == nil); err != nil {
		return serviceConfig{}, err
	}
	listen, present := members["listen"]
	var written string
	if !present || json.Unmarshal(listen, &written) != nil {
		return serviceConfig{}, errors.New("listen must be the address Runner listens on, an IP address and a port, as in 127.0.0.1:8470")
	}
	if c.listen, err = listenAddress(written); err != nil {
		return serviceConfig{}, err
	}
	accept := false
	if v, present := members["acceptOtherHostsOverPlainHTTP"]; present {
		switch string(v) {
		case "true":
			accept = true
		case "false":
		default:
			return serviceConfig{}, errors.New("acceptOtherHostsOverPlainHTTP, when present, is true or false")
		}
	}
	if err = otherHosts(c.listen, c.identity != nil, accept); err != nil {
		return serviceConfig{}, err
	}
	if err = runner.ServiceConfigOutsideState(path, c.runner.Dir); err != nil {
		return serviceConfig{}, fmt.Errorf("the service configuration is refused: %w", err)
	}
	return c, nil
}

// serviceObject reads a service configuration as one JSON object, each member
// once at every depth, as the boot line is read (uniqueJSONValue).
func serviceObject(raw []byte) (map[string]json.RawMessage, error) {
	refused := errors.New("the service configuration is not one JSON object with each member once")
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if uniqueJSONValue(decoder, 0) != nil {
		return nil, refused
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, refused
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || members == nil {
		return nil, refused
	}
	return members, nil
}

// listenAddress holds listen to an IP address and a port, written exactly as
// Runner binds and prints it: IPv4 in dotted decimal, IPv6 in brackets,
// compressed and in lowercase, an IPv4 address never in IPv6 form, the port a
// number from 1 to 65535 with no sign and no leading zero. A name, localhost
// included, a zone and an empty host are refused, so no resolver decides where
// Runner listens; port 0, which the system would choose, is refused, since a
// service is found at the port its operator chose. Any other spelling is
// refused. A refusal says what is wrong, never repeating the value, as no
// refusal of the configuration repeats what it holds.
func listenAddress(listen string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return netip.AddrPort{}, errors.New("listen is not an IP address and a port")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" {
		return netip.AddrPort{}, errors.New("listen is not an IP address and a port: IPv4 in dotted decimal, or IPv6 in brackets, with no zone; a name is not one, localhost included")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return netip.AddrPort{}, errors.New("listen has no port from 1 to 65535: a service listens at the port its operator chose, never one the system picks")
	}
	canonical := netip.AddrPortFrom(addr.Unmap(), uint16(n))
	if canonical.String() != listen {
		return netip.AddrPort{}, errors.New("listen is an address written another way than Runner writes it: IPv4 in dotted decimal, never in IPv6 form; IPv6 in brackets, compressed and in lower case; the port with no sign and no leading zero")
	}
	return canonical, nil
}

// otherHosts holds a listen address to who can reach it, as the gateway's
// serve --config holds its own (its ADR-0025): a loopback address
// (127.0.0.0/8 or ::1) is taken, and acceptOtherHostsOverPlainHTTP true
// beside one is refused, so the acknowledgment is never in a file with
// nothing to acknowledge. Any other address is taken only with an identity,
// by which every request but GET /v1/health holds an identity token, and
// acceptOtherHostsOverPlainHTTP true, which acknowledges that Runner speaks
// plain HTTP: either alone is refused, so a changed address alone never
// exposes the control API.
func otherHosts(at netip.AddrPort, identity, accept bool) error {
	if at.Addr().IsLoopback() {
		if accept {
			return errors.New("acceptOtherHostsOverPlainHTTP is true, and listen is a loopback address: the member acknowledges a listen that is not loopback, and is true only beside one")
		}
		return nil
	}
	var missing string
	switch {
	case !identity && !accept:
		missing = "this configuration has neither"
	case !identity:
		missing = "this configuration has no identity"
	case !accept:
		missing = "this configuration does not set acceptOtherHostsOverPlainHTTP to true"
	default:
		return nil
	}
	return fmt.Errorf("listen is not a loopback address: other hosts can reach it, so Runner listens there only with identity, "+
		"by which every request but GET /v1/health holds an identity token, and acceptOtherHostsOverPlainHTTP: true, "+
		"which acknowledges that the tokens cross plain HTTP unless TLS terminates in front of Runner, which terminates none; %s", missing)
}

// otherHostsNote is the start's line on standard error for a listener other
// hosts can reach, naming the address bound.
func otherHostsNote(addr net.Addr) string {
	return fmt.Sprintf("listening on %s, which is not a loopback address: other hosts can reach this Runner; "+
		"it holds every request but GET /v1/health to an identity token, and speaks plain HTTP, "+
		"so unless TLS terminates in front of it a token can be read and replayed on the network between", addr)
}

// serveConfigured runs Runner in service mode until ctx is done: the
// configuration at path read and held, then the identity's key set, before
// anything is opened; then the store opened, the notes a companion writes
// written on standard error, the listen address bound in its own family, the
// note for an address other hosts can reach, and the readiness line a
// companion writes on standard output.
func serveConfigured(ctx context.Context, path string, stdout, stderr io.Writer) error {
	c, err := readServiceConfig(path)
	if err != nil {
		return err
	}
	var identity *runner.Identity
	if c.identity != nil {
		if identity, err = runner.LoadIdentity(*c.identity, c.runner.Owner, c.runner.Dir); err != nil {
			return err
		}
	}
	service, err := runner.Open(c.runner)
	if err != nil {
		return err
	}
	defer service.Close()
	for _, p := range service.SetAside() {
		fmt.Fprintln(stderr, "runner:", p)
	}
	if note := service.InputRootBinding().String(); note != "" {
		fmt.Fprintln(stderr, "runner:", note)
	}
	network := "tcp4"
	if c.listen.Addr().Is6() {
		network = "tcp6"
	}
	listener, err := net.Listen(network, c.listen.String())
	if err != nil {
		return err
	}
	if tcp, ok := listener.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		fmt.Fprintln(stderr, "runner:", otherHostsNote(listener.Addr()))
	}
	server := controlServer(service.ServiceHandler(c.token, identity))
	defer server.Close()
	if err = json.NewEncoder(stdout).Encode(map[string]string{"url": "http://" + listener.Addr().String(), "protocol": "jobs/1"}); err != nil {
		listener.Close()
		return err
	}
	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener) }()
	select {
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
