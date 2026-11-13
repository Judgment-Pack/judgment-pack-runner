//go:build linux || darwin || freebsd

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Service mode (service.go, docs/design/service-mode.md): its configuration
// file, its refusals, which precede every effect of a start, and a start that
// serves until it is stopped.

const serviceBearer = "service-test-bearer-0f9e8d7c6b5a49382716"

// serviceFixture is a directory of the test's own, named by its real path,
// with a stand-in Runtime that no test runs and a state directory that does
// not exist until Runner opens it.
type serviceFixture struct {
	t                   *testing.T
	dir, state, runtime string
}

func newServiceFixture(t *testing.T) serviceFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(dir, "never-run")
	if err = os.WriteFile(runtime, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return serviceFixture{t, dir, filepath.Join(dir, "state"), runtime}
}

// members are a service configuration's members: the boot members a
// companion's line would carry, the owner bearer and listen, with changes
// set, and those whose value is nil removed.
func (f serviceFixture) members(listen string, changes map[string]any) map[string]any {
	m := map[string]any{"dir": f.state, "runtime": f.runtime, "workspace": "service-workspace", "owner": "service-owner", "token": serviceBearer, "listen": listen}
	for k, v := range changes {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m
}

// write writes a service configuration's bytes, private to the test's user,
// and returns its path.
func (f serviceFixture) write(name string, raw []byte) string {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f serviceFixture) config(name string, members map[string]any) string {
	f.t.Helper()
	raw, err := json.Marshal(members)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.write(name, raw)
}

// identityFixture is an identity of one generated Ed25519 key, whose key set
// file it writes beside the configuration.
type identityFixture struct {
	private ed25519.PrivateKey
	member  map[string]any
}

func (f serviceFixture) identity(subjects ...string) identityFixture {
	f.t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	set, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "kid": "k1", "x": base64.RawURLEncoding.EncodeToString(public)}}})
	keys := filepath.Join(f.dir, "keys.json")
	if err = os.WriteFile(keys, set, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return identityFixture{private, map[string]any{"issuer": "https://issuer.example", "audience": "runner-control", "subjects": subjects, "keys": keys}}
}

// token is an identity token for subject, valid for five minutes.
func (id identityFixture) token(subject string) string {
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"EdDSA","kid":"k1","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iss": "https://issuer.example", "aud": "runner-control", "sub": subject, "exp": time.Now().Add(5 * time.Minute).Unix()})
	signed := header + "." + enc(claims)
	return signed + "." + enc(ed25519.Sign(id.private, []byte(signed)))
}

// freeLoopbackPort is a port no listener held on 127.0.0.1 a moment ago.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// Every refusal of a service configuration is said on standard error before
// anything is opened: the state directory is never made, nothing is written
// on standard output, standard input is never read, and no refusal repeats
// the bearer or a path.
func TestAServiceConfigurationIsRefusedBeforeAnyEffect(t *testing.T) {
	// A configuration a mutant would take starts on a port no one holds, and
	// is stopped by refusedWithin.
	loopback := fmt.Sprintf("127.0.0.1:%d", freeLoopbackPort(t))
	cases := []struct {
		name string
		path func(f serviceFixture) string
		want string
	}{
		{"a configuration others can read", func(f serviceFixture) string {
			p := f.config("service.json", f.members(loopback, nil))
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}, "the service configuration is refused: neither its group nor others may read or write it"},
		{"a configuration by a symbolic link", func(f serviceFixture) string {
			p := f.config("service.json", f.members(loopback, nil))
			link := filepath.Join(f.dir, "link.json")
			if err := os.Symlink(p, link); err != nil {
				t.Fatal(err)
			}
			return link
		}, "the service configuration is refused: no symbolic link"},
		{"a member twice", func(f serviceFixture) string {
			raw, _ := json.Marshal(f.members(loopback, nil))
			return f.write("service.json", append(bytes.TrimSuffix(raw, []byte("}")), []byte(`,"listen":"0.0.0.0:8470"}`)...))
		}, "the service configuration is not one JSON object with each member once"},
		{"not an object", func(f serviceFixture) string { return f.write("service.json", []byte(`[]`)) }, "not one JSON object"},
		{"a member Runner does not read", func(f serviceFixture) string {
			return f.config("service.json", f.members(loopback, map[string]any{"listenAddress": loopback}))
		}, `a member Runner does not read, "listenAddress"`},
		{"a member's name in another case", func(f serviceFixture) string {
			return f.config("service.json", f.members("", map[string]any{"listen": nil, "Listen": loopback}))
		}, `member "Listen" is listen spelled another way`},
		{"a boot member's name in another case", func(f serviceFixture) string {
			return f.config("service.json", f.members(loopback, map[string]any{"Owner": "another-owner"}))
		}, `member "Owner" is owner spelled another way`},
		{"executor ownership", func(f serviceFixture) string {
			return f.config("service.json", f.members(loopback, map[string]any{"executorOwnership": map[string]any{}}))
		}, "executorOwnership binds Runner to the host that starts it"},
		{"executor admission", func(f serviceFixture) string {
			return f.config("service.json", f.members(loopback, map[string]any{"executorAdmission": map[string]any{}}))
		}, "executorAdmission binds Runner to the host that starts it"},
		{"no owner bearer and no identity", func(f serviceFixture) string {
			return f.config("service.json", f.members(loopback, map[string]any{"token": nil}))
		}, "token must be the owner bearer, at least 32 characters"},
		{"a short owner bearer", func(f serviceFixture) string {
			return f.config("service.json", f.members(loopback, map[string]any{"token": "short"}))
		}, "token must be the owner bearer, at least 32 characters"},
		{"an owner bearer beside an identity", func(f serviceFixture) string {
			return f.config("service.json", f.members(loopback, map[string]any{"identity": f.identity("ops-service").member}))
		}, "token is the owner bearer, which a configuration with identity does not take"},
		{"an identity without keys", func(f serviceFixture) string {
			id := f.identity("ops-service").member
			delete(id, "keys")
			return f.config("service.json", f.members(loopback, map[string]any{"token": nil, "identity": id}))
		}, "identity.keys must be the absolute, clean path"},
		{"an identity whose key set is not there", func(f serviceFixture) string {
			id := f.identity("ops-service").member
			id["keys"] = filepath.Join(f.dir, "absent.json")
			return f.config("service.json", f.members(loopback, map[string]any{"token": nil, "identity": id}))
		}, "identity.keys is refused: there is no file at its path"},
		{"an identity that lists the owner", func(f serviceFixture) string {
			return f.config("service.json", f.members(loopback, map[string]any{"token": nil, "identity": f.identity("ops-service", "service-owner").member}))
		}, "identity.subjects[1] is, or with the issuer names, the installation's owner"},
		{"no listen", func(f serviceFixture) string {
			return f.config("service.json", f.members("", map[string]any{"listen": nil}))
		}, "listen must be the address Runner listens on"},
		{"a listen that is a number", func(f serviceFixture) string {
			return f.config("service.json", f.members("", map[string]any{"listen": 8470}))
		}, "listen must be the address Runner listens on"},
		{"a name", func(f serviceFixture) string { return f.config("service.json", f.members("localhost:8470", nil)) }, "a name is not one, localhost included"},
		{"no host", func(f serviceFixture) string { return f.config("service.json", f.members(":8470", nil)) }, "a name is not one, localhost included"},
		{"no port", func(f serviceFixture) string { return f.config("service.json", f.members("127.0.0.1", nil)) }, "is not an IP address and a port"},
		{"a zone", func(f serviceFixture) string { return f.config("service.json", f.members("[fe80::1%lo]:8470", nil)) }, "with no zone"},
		{"port 0", func(f serviceFixture) string { return f.config("service.json", f.members("127.0.0.1:0", nil)) }, "never one the system picks"},
		{"a port past 65535", func(f serviceFixture) string { return f.config("service.json", f.members("127.0.0.1:65536", nil)) }, "has no port from 1 to 65535"},
		{"a port with a leading zero", func(f serviceFixture) string { return f.config("service.json", f.members("127.0.0.1:08470", nil)) }, "written another way than Runner writes it"},
		{"IPv4 in IPv6 form", func(f serviceFixture) string {
			return f.config("service.json", f.members("[::ffff:127.0.0.1]:8470", nil))
		}, "written another way than Runner writes it"},
		{"IPv6 uncompressed", func(f serviceFixture) string {
			return f.config("service.json", f.members("[0:0:0:0:0:0:0:1]:8470", nil))
		}, "written another way than Runner writes it"},
		{"another host, with neither", func(f serviceFixture) string { return f.config("service.json", f.members("192.0.2.10:8470", nil)) }, "this configuration has neither"},
		{"another host, without identity", func(f serviceFixture) string {
			return f.config("service.json", f.members("192.0.2.10:8470", map[string]any{"acceptOtherHostsOverPlainHTTP": true}))
		}, "this configuration has no identity"},
		{"another host, without acknowledgment", func(f serviceFixture) string {
			return f.config("service.json", f.members("[2001:db8::10]:8470", map[string]any{"token": nil, "identity": f.identity("ops-service").member}))
		}, "this configuration does not set acceptOtherHostsOverPlainHTTP to true"},
		{"another host, acknowledged as false", func(f serviceFixture) string {
			return f.config("service.json", f.members("192.0.2.10:8470", map[string]any{"token": nil, "identity": f.identity("ops-service").member, "acceptOtherHostsOverPlainHTTP": false}))
		}, "this configuration does not set acceptOtherHostsOverPlainHTTP to true"},
		{"an acknowledgment beside loopback", func(f serviceFixture) string {
			return f.config("service.json", f.members(loopback, map[string]any{"acceptOtherHostsOverPlainHTTP": true}))
		}, "is true only beside one"},
		{"an acknowledgment that is not a boolean", func(f serviceFixture) string {
			return f.config("service.json", f.members("192.0.2.10:8470", map[string]any{"acceptOtherHostsOverPlainHTTP": "true"}))
		}, "acceptOtherHostsOverPlainHTTP, when present, is true or false"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newServiceFixture(t)
			path := c.path(f)
			status, stdout, stderr := refusedWithin(t, path)
			if status == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), c.want) {
				t.Fatalf("status=%d stdout=%q stderr=%q, want %q", status, stdout.String(), stderr.String(), c.want)
			}
			held := []string{serviceBearer, f.dir}
			// Nor the listen address the configuration gives.
			var written struct {
				Listen any `json:"listen"`
			}
			if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &written) == nil {
				if listen, ok := written.Listen.(string); ok && listen != "" {
					held = append(held, listen)
				}
			}
			for _, held := range held {
				if strings.Contains(stderr.String(), held) {
					t.Fatalf("the refusal repeats %q: %s", held, stderr.String())
				}
			}
			if _, err := os.Stat(f.state); !os.IsNotExist(err) {
				t.Fatalf("a refused configuration made the state directory: %v", err)
			}
		})
	}
}

// refusedWithin runs serve with the configuration at path, which it must
// refuse at once: a start it does not refuse is stopped, with the process's
// own interrupt, and fails the test, rather than serving until the test
// binary is stopped.
func refusedWithin(t *testing.T, path string) (int, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runnerCommand([]string{"serve", "--config", path}, unreadableBoot{}, &stdout, &stderr) }()
	select {
	case status := <-done:
		return status, &stdout, &stderr
	case <-time.After(20 * time.Second):
		p, err := os.FindProcess(os.Getpid())
		if err == nil {
			p.Signal(os.Interrupt)
		}
		<-done
		t.Fatalf("the configuration was not refused: Runner started: %s", stderr.String())
		return 0, nil, nil
	}
}

// A service configuration inside Runner's state directory is refused, as the
// signing key is.
func TestAServiceConfigurationInsideTheStateDirectoryIsRefused(t *testing.T) {
	f := newServiceFixture(t)
	if err := os.Mkdir(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	path := f.config("state/service.json", f.members(fmt.Sprintf("127.0.0.1:%d", freeLoopbackPort(t)), nil))
	if status, stdout, stderr := refusedWithin(t, path); status == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "it is inside Runner's state directory") {
		t.Fatalf("status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if entries, err := os.ReadDir(f.state); err != nil || len(entries) != 1 {
		t.Fatalf("Runner opened its store: %v %v", entries, err)
	}
}

// serve takes --config and one path, and nothing else, and reads no stdin.
func TestServeTakesItsConfigurationAndNothingElse(t *testing.T) {
	for _, args := range [][]string{{"serve"}, {"serve", "--config"}, {"serve", "--config", "/a", "/b"}, {"serve", "--Config", "/a"}, {"serve", "--config=/a"}, {"serve", "/a"}} {
		var stdout, stderr bytes.Buffer
		if status := runnerCommand(args, unreadableBoot{}, &stdout, &stderr); status == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "serve takes --config and the absolute path of its service configuration") {
			t.Fatalf("%v: status=%d stdout=%q stderr=%q", args, status, stdout.String(), stderr.String())
		}
	}
}

// A configuration that listens where other hosts can reach it is taken with
// an identity and the acknowledgment, both, and holds no owner bearer.
func TestAnotherHostIsTakenWithIdentityAndAcknowledgment(t *testing.T) {
	f := newServiceFixture(t)
	for _, listen := range []string{"0.0.0.0:8470", "192.0.2.10:8470", "[::]:8470", "[2001:db8::10]:8470"} {
		path := f.config("service.json", f.members(listen, map[string]any{"token": nil, "identity": f.identity("ops-service").member, "acceptOtherHostsOverPlainHTTP": true}))
		c, err := readServiceConfig(path)
		if err != nil || c.listen.String() != listen || c.identity == nil || c.token != "" || c.identity.Subjects[0] != "ops-service" {
			t.Fatalf("%s: %+v %v", listen, c, err)
		}
	}
	if note := otherHostsNote(&net.TCPAddr{IP: net.IPv4zero, Port: 8470}); !strings.Contains(note, "0.0.0.0:8470, which is not a loopback address") || !strings.Contains(note, "plain HTTP") {
		t.Fatal(note)
	}
}

// startService runs serveConfigured with the configuration at path until the
// test stops it, and returns the URL its readiness line names and the stop.
func startService(t *testing.T, path string) (string, func() (string, error)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out, in := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		err := serveConfigured(ctx, path, in, &stderr)
		in.Close()
		done <- err
	}()
	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		lines <- line
		io.Copy(io.Discard, out)
	}()
	var line string
	select {
	case line = <-lines:
	case err := <-done:
		cancel()
		t.Fatalf("the service did not start: %v %s", err, stderr.String())
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("the service wrote no readiness line")
	}
	var ready map[string]string
	if err := json.Unmarshal([]byte(line), &ready); err != nil || len(ready) != 2 || ready["protocol"] != "jobs/1" {
		cancel()
		t.Fatalf("readiness line %q: %v", line, err)
	}
	stopped := false
	stop := func() (string, error) {
		t.Helper()
		if stopped {
			return stderr.String(), nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return stderr.String(), err
		case <-time.After(40 * time.Second):
			t.Fatal("the service did not stop")
		}
		return "", nil
	}
	t.Cleanup(func() { stop() })
	return ready["url"], stop
}

// get serves one request on the service, with a bearer when bearer is not "".
func get(t *testing.T, url, bearer string) (int, http.Header, string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header, string(body)
}

// A service listens at the port its operator chose, on loopback, holds every
// request but GET /v1/health to the owner bearer, and serves until it is
// stopped, reading no stdin; it says its readiness as a companion does.
func TestAServiceServesUntilItIsStopped(t *testing.T) {
	f := newServiceFixture(t)
	port := freeLoopbackPort(t)
	url, stop := startService(t, f.config("service.json", f.members(fmt.Sprintf("127.0.0.1:%d", port), nil)))
	if url != fmt.Sprintf("http://127.0.0.1:%d", port) {
		t.Fatalf("readiness names %s, not the port chosen", url)
	}
	if code, _, body := get(t, url+"/v1/health", ""); code != 200 || body != "{\"healthy\":true}\n" {
		t.Fatalf("health: %d %q", code, body)
	}
	if code, _, body := get(t, url+"/v1/status", ""); code != 401 || body != "{\"message\":\"Runner authentication required.\"}\n" {
		t.Fatalf("status without the bearer: %d %q", code, body)
	}
	if code, _, body := get(t, url+"/v1/status", serviceBearer); code != 200 || !strings.Contains(body, `"owner":"service-owner"`) {
		t.Fatalf("status: %d %q", code, body)
	}
	stderr, err := stop()
	if err != nil || strings.Contains(stderr, serviceBearer) {
		t.Fatalf("%v %q", err, stderr)
	}
}

// With an identity, a service holds every request but GET /v1/health to an
// identity token, and takes no owner bearer.
func TestAServiceWithAnIdentityHoldsEveryRequestToAToken(t *testing.T) {
	f := newServiceFixture(t)
	id := f.identity("ops-service")
	port := freeLoopbackPort(t)
	url, stop := startService(t, f.config("service.json", f.members(fmt.Sprintf("127.0.0.1:%d", port), map[string]any{"token": nil, "identity": id.member})))
	if code, _, body := get(t, url+"/v1/health", ""); code != 200 || body != "{\"healthy\":true}\n" {
		t.Fatalf("health: %d %q", code, body)
	}
	for name, bearer := range map[string]string{"no token": "", "the owner bearer's shape": serviceBearer, "a subject not listed": id.token("build-bot")} {
		if code, header, body := get(t, url+"/v1/status", bearer); code != 401 || header.Get("WWW-Authenticate") != "Bearer" || !strings.HasPrefix(body, `{"message":"Runner authentication required: `) {
			t.Fatalf("%s: %d %v %q", name, code, header, body)
		}
	}
	token := id.token("ops-service")
	code, _, body := get(t, url+"/v1/status", token)
	if code != 200 || !strings.Contains(body, `"owner":"service-owner"`) || strings.Contains(body, token) {
		t.Fatalf("status: %d %q", code, body)
	}
	stderr, err := stop()
	if err != nil || strings.Contains(stderr, token) {
		t.Fatalf("%v %q", err, stderr)
	}
}
