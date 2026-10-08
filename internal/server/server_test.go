package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SRS-Hosting/rcon-web/internal/config"
	"github.com/SRS-Hosting/rcon/rcontest"
)

const loopback = "127.0.0.1"

func newTestRCON(t *testing.T) *rcontest.Server {
	t.Helper()
	srv, err := rcontest.New(rcontest.Respond("pw", 0, func(command string) string {
		switch command {
		case "status":
			return "hostname: test\nplayers: 0"
		case "silent":
			return ""
		case "slow":
			time.Sleep(3 * time.Second)
			return "too late"
		case "xss":
			return "<script>alert(1)</script>"
		default:
			return "Unknown command"
		}
	}))
	if err != nil {
		t.Fatalf("start fake rcon server: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, int(port)
}

func newTestServer(t *testing.T, rconAddr, password string, timeoutSeconds int) *Server {
	t.Helper()
	host, port := splitAddr(t, rconAddr)
	s, err := New(&config.Config{
		LogLevel: config.LogLevelInfo,
		HTTP:     config.HTTP{Bind: loopback, Port: 8080},
		RCON:     config.RCON{Host: host, Port: port, Password: password, TimeoutSeconds: timeoutSeconds, MaxConcurrent: 4},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// get and postJSON exist so the tests go through http.NewRequestWithContext:
// a test that hangs should fail on its own deadline, not wait forever.
func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func postJSON(ctx context.Context, url, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func post(t *testing.T, s *Server, body string) (int, commandResponse) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/command", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	var got commandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, got
}

func TestCommandHappyPath(t *testing.T) {
	rs := newTestRCON(t)
	s := newTestServer(t, rs.Addr(), "pw", 5)

	code, got := post(t, s, `{"command":"status"}`)
	if code != http.StatusOK || got.Output != "hostname: test\nplayers: 0" {
		t.Fatalf("status: got %d %+v", code, got)
	}

	code, got = post(t, s, `{"command":"nope"}`)
	if code != http.StatusOK || got.Output != "Unknown command" {
		t.Fatalf("unknown: got %d %+v", code, got)
	}

	// Empty responses are normal for many RCON commands and must not be errors.
	code, got = post(t, s, `{"command":"silent"}`)
	if code != http.StatusOK || got.Output != "" || got.Error != "" {
		t.Fatalf("silent: got %d %+v", code, got)
	}

	// Markup must survive verbatim in JSON; the page renders it as text.
	code, got = post(t, s, `{"command":"xss"}`)
	if code != http.StatusOK || got.Output != "<script>alert(1)</script>" {
		t.Fatalf("xss: got %d %+v", code, got)
	}
}

func TestCommandTimeout(t *testing.T) {
	rs := newTestRCON(t)
	s := newTestServer(t, rs.Addr(), "pw", 1)

	start := time.Now()
	code, got := post(t, s, `{"command":"slow"}`)
	elapsed := time.Since(start)

	if code != http.StatusGatewayTimeout {
		t.Fatalf("got %d %+v", code, got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %s, expected the 1s deadline to bound it", elapsed)
	}
	if !strings.Contains(got.Error, "did not respond within 1s") {
		t.Fatalf("unclear error: %q", got.Error)
	}
	t.Logf("timed out after %s: %s", elapsed.Round(time.Millisecond), got.Error)
}

func TestBadPassword(t *testing.T) {
	rs := newTestRCON(t)
	s := newTestServer(t, rs.Addr(), "wrong", 5)

	code, got := post(t, s, `{"command":"status"}`)
	if code != http.StatusBadGateway || !strings.Contains(got.Error, "authentication failed") {
		t.Fatalf("got %d %+v", code, got)
	}
	t.Logf("auth: %s", got.Error)
}

// TestConnectionRefused: an immediately-refused connect, distinct from the
// black-hole stall covered by TestTimeoutDuringAuth.
func TestConnectionRefused(t *testing.T) {
	// Port 1 on loopback refuses connections immediately.
	s := newTestServer(t, "127.0.0.1:1", "pw", 5)

	start := time.Now()
	code, got := post(t, s, `{"command":"status"}`)
	if code != http.StatusBadGateway || got.Error == "" {
		t.Fatalf("got %d %+v", code, got)
	}
	t.Logf("refused after %s: %s", time.Since(start).Round(time.Millisecond), got.Error)
}

func TestBadRequests(t *testing.T) {
	rs := newTestRCON(t)
	s := newTestServer(t, rs.Addr(), "pw", 5)

	for _, body := range []string{`{"command":""}`, `not json`, `{"cmd":"status"}`} {
		code, got := post(t, s, body)
		if code != http.StatusBadRequest || got.Error == "" {
			t.Fatalf("body %q: got %d %+v", body, code, got)
		}
	}

	code, got := post(t, s, `{"command":"`+strings.Repeat("a", 1001)+`"}`)
	if code != http.StatusBadRequest || !strings.Contains(got.Error, "at most 1000") {
		t.Fatalf("long command: got %d %+v", code, got)
	}
	t.Logf("long command: %s", got.Error)
}

func TestRoutes(t *testing.T) {
	rs := newTestRCON(t)
	s := newTestServer(t, rs.Addr(), "pw", 5)

	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "RCON Console") {
		t.Fatalf("index: %d", rec.Code)
	}
	if !strings.Contains(body, rs.Addr()) {
		t.Fatal("page does not name the target server")
	}
	// html/template pads values injected into a script context with spaces.
	if !strings.Contains(body, "TIMEOUT_MS =  5  * 1000") {
		t.Fatal("timeout was not injected into the page script")
	}
	if !strings.Contains(body, "time out after 5s") {
		t.Fatal("timeout was not injected into the hint text")
	}
	// The page must carry no external references, or it will not work offline.
	for _, bad := range []string{"http://", "https://", "src=", "href=", "@import", "url(", "@font-face", "integrity=", "crossorigin"} {
		if strings.Contains(body, bad) {
			t.Errorf("page references something external: %q", bad)
		}
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/nope"},
		{http.MethodGet, "/api/command"},
		{http.MethodPost, "/"},
	} {
		rec := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s unexpectedly returned 200", tc.method, tc.path)
		}
		t.Logf("%s %s -> %d", tc.method, tc.path, rec.Code)
	}
}

// TestDualStack is the point of the explicit two-listener setup: the same port
// must answer over both IPv4 and IPv6 loopback.
//
// It skips where IPv6 is unavailable rather than failing. The production code
// deliberately degrades to one family there, so failing would contradict the
// behaviour the rest of this file asserts.
func TestDualStack(t *testing.T) {
	var probe net.ListenConfig
	if ln, err := probe.Listen(context.Background(), "tcp6", "[::1]:0"); err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	} else {
		_ = ln.Close()
	}

	rs := newTestRCON(t)
	host, port := splitAddr(t, rs.Addr())
	s, err := New(&config.Config{
		LogLevel: config.LogLevelInfo,
		HTTP:     config.HTTP{Bind: "", Port: 18099},
		RCON:     config.RCON{Host: host, Port: port, Password: "pw", TimeoutSeconds: 5, MaxConcurrent: 4},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	waitUp(t, "http://127.0.0.1:18099/")

	for _, url := range []string{"http://127.0.0.1:18099/", "http://[::1]:18099/"} {
		resp, err := get(context.Background(), url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		closeBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", url, resp.StatusCode)
		}
		t.Logf("GET %s -> %d", url, resp.StatusCode)
	}
}

func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
}

func waitUp(t *testing.T, url string) {
	t.Helper()
	var err error
	for range 200 {
		var resp *http.Response
		resp, err = get(context.Background(), url)
		if err == nil {
			closeBody(t, resp)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never came up at %s: %v", url, err)
}

func TestGracefulShutdown(t *testing.T) {
	rs := newTestRCON(t)
	host, port := splitAddr(t, rs.Addr())
	s, err := New(&config.Config{
		LogLevel: config.LogLevelInfo,
		HTTP:     config.HTTP{Bind: loopback, Port: 18098},
		RCON:     config.RCON{Host: host, Port: port, Password: "pw", TimeoutSeconds: 5, MaxConcurrent: 4},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitUp(t, "http://127.0.0.1:18098/")

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
		t.Logf("shut down cleanly in %s", time.Since(start).Round(time.Millisecond))
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	// The port must actually be released.
	if _, err := get(context.Background(), "http://127.0.0.1:18098/"); err == nil {
		t.Fatal("server still accepting connections after shutdown")
	}
}

// TestInflightRequestSurvivesShutdown checks the drain: a command already
// running when the signal arrives must still get its response.
func TestInflightRequestSurvivesShutdown(t *testing.T) {
	rs := newTestRCON(t)
	host, port := splitAddr(t, rs.Addr())
	s, err := New(&config.Config{
		LogLevel: config.LogLevelInfo,
		HTTP:     config.HTTP{Bind: loopback, Port: 18097},
		RCON:     config.RCON{Host: host, Port: port, Password: "pw", TimeoutSeconds: 5, MaxConcurrent: 4},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitUp(t, "http://127.0.0.1:18097/")

	// "slow" sleeps 3s upstream, well inside the 5s RCON timeout.
	type res struct {
		code int
		err  error
	}
	reqDone := make(chan res, 1)
	go func() {
		resp, err := postJSON(context.Background(),
			"http://127.0.0.1:18097/api/command", `{"command":"slow"}`)
		if err != nil {
			reqDone <- res{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		reqDone <- res{code: resp.StatusCode}
	}()

	time.Sleep(300 * time.Millisecond) // let the request reach the RCON call
	cancel()

	select {
	case r := <-reqDone:
		if r.err != nil {
			t.Fatalf("in-flight request was dropped: %v", r.err)
		}
		if r.code != http.StatusOK {
			t.Fatalf("in-flight request got %d, want 200", r.code)
		}
		t.Log("in-flight request completed during drain")
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight request never finished")
	}

	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestListenFallsBackToOneFamily covers a host where one stack is unavailable:
// holding the IPv6 wildcard makes the tcp6 bind fail, and the service must still
// come up on IPv4 rather than refusing to start. Occupying "[::]" with Go's tcp6
// network sets IPV6_V6ONLY, so it does not also take the IPv4 wildcard.
func TestListenFallsBackToOneFamily(t *testing.T) {
	var lc net.ListenConfig
	blocker, err := lc.Listen(context.Background(), "tcp6", "[::]:18096")
	if err != nil {
		t.Skipf("cannot hold the IPv6 wildcard on this host: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	rs := newTestRCON(t)
	host, port := splitAddr(t, rs.Addr())
	s, err := New(&config.Config{
		LogLevel: config.LogLevelInfo,
		HTTP:     config.HTTP{Bind: "", Port: 18096},
		RCON:     config.RCON{Host: host, Port: port, Password: "pw", TimeoutSeconds: 5, MaxConcurrent: 4},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	listeners, err := s.listen(context.Background())
	if err != nil {
		t.Fatalf("listen should have fallen back to IPv4, got: %v", err)
	}
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()

	if len(listeners) != 1 {
		t.Fatalf("got %d listeners, want 1 (IPv4 only)", len(listeners))
	}
	if got := listeners[0].Addr().String(); got != "0.0.0.0:18096" {
		t.Fatalf("fell back to %s, want the IPv4 wildcard", got)
	}
	t.Logf("degraded to a single family: %s", listeners[0].Addr())
}

// TestListenBothFamiliesUnavailable is the other half of the policy: degrading is
// fine, but a port that is unusable on every family is still a hard failure.
func TestListenBothFamiliesUnavailable(t *testing.T) {
	var lc net.ListenConfig
	v4, err := lc.Listen(context.Background(), "tcp4", "0.0.0.0:18095")
	if err != nil {
		t.Skipf("cannot hold the IPv4 wildcard: %v", err)
	}
	defer func() { _ = v4.Close() }()
	v6, err := lc.Listen(context.Background(), "tcp6", "[::]:18095")
	if err != nil {
		t.Skipf("cannot hold the IPv6 wildcard: %v", err)
	}
	defer func() { _ = v6.Close() }()

	s, err := New(&config.Config{
		LogLevel: config.LogLevelInfo,
		HTTP:     config.HTTP{Bind: "", Port: 18095},
		RCON:     config.RCON{Host: loopback, Port: 27015, Password: "pw", TimeoutSeconds: 5, MaxConcurrent: 4},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := s.listen(context.Background()); err == nil {
		t.Fatal("expected an error when no family could bind")
	} else {
		t.Logf("hard failure when nothing could bind: %v", err)
	}
}

func TestPortInUse(t *testing.T) {
	blocker := httptest.NewServer(http.NotFoundHandler())
	defer blocker.Close()
	_, port := splitAddr(t, strings.TrimPrefix(blocker.URL, "http://"))

	s, err := New(&config.Config{
		LogLevel: config.LogLevelInfo,
		HTTP:     config.HTTP{Bind: loopback, Port: port},
		RCON:     config.RCON{Host: loopback, Port: 27015, Password: "pw", TimeoutSeconds: 5, MaxConcurrent: 4},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a bind error")
		}
		t.Logf("bind failure surfaced: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run swallowed the bind error")
	}
}

// TestRejectsNonJSONContentType is the CSRF guard. A cross-origin fetch with
// application/json must be preflighted by the browser, and an HTML form cannot
// produce that type at all, so refusing everything else is what stops a hostile
// page from making an authenticated victim run RCON commands.
func TestRejectsNonJSONContentType(t *testing.T) {
	rs := newTestRCON(t)
	s := newTestServer(t, rs.Addr(), "pw", 5)

	// Exactly the types a cross-origin form can send without a preflight.
	for _, ct := range []string{
		"text/plain",
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"",
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/command", strings.NewReader(`{"command":"status"}`))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q got %d, want 415 (command would have executed)", ct, rec.Code)
			continue
		}
		t.Logf("Content-Type %q -> 415", ct)
	}

	// A charset parameter is legitimate and must still be accepted.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/command", strings.NewReader(`{"command":"status"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("application/json; charset=utf-8 got %d, want 200", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	rs := newTestRCON(t)
	s := newTestServer(t, rs.Addr(), "pw", 5)

	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	for header, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "frame-ancestors 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing %q: %s", directive, csp)
		}
	}
}

func TestOversizedBody(t *testing.T) {
	rs := newTestRCON(t)
	s := newTestServer(t, rs.Addr(), "pw", 5)

	code, got := post(t, s, `{"command":"`+strings.Repeat("a", maxRequestBody*2)+`"}`)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d %+v, want 413", code, got)
	}
	t.Logf("oversized body: %s", got.Error)
}

// TestLongCommandWithDeadServer proves the length check is local: with the RCON
// server unreachable, an over-long command must still be reported as a bad
// request rather than as a connection failure.
func TestLongCommandWithDeadServer(t *testing.T) {
	s := newTestServer(t, "127.0.0.1:1", "pw", 5)

	code, got := post(t, s, `{"command":"`+strings.Repeat("a", 1001)+`"}`)
	if code != http.StatusBadRequest || !strings.Contains(got.Error, "at most 1000 bytes") {
		t.Fatalf("got %d %+v, want 400 about the length", code, got)
	}
	t.Logf("length rejected without dialing: %s", got.Error)
}

// blackHole accepts connections and never speaks, which stalls the RCON exchange
// during authentication rather than after it. It reports when each accepted
// connection is closed, so a test can check who closed it and when.
func blackHole(t *testing.T) (addr string, closed <-chan time.Duration) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("black hole listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	ch := make(chan time.Duration, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				accepted := time.Now()
				// Read until the peer gives up or goes away; never reply.
				_, _ = io.Copy(io.Discard, conn)
				ch <- time.Since(accepted)
				_ = conn.Close()
			}(conn)
		}
	}()
	return ln.Addr().String(), ch
}

// TestTimeoutDuringAuth covers the stall the other timeout test does not: a
// server that accepts the TCP connection and then never answers the auth packet.
func TestTimeoutDuringAuth(t *testing.T) {
	addr, closed := blackHole(t)
	s := newTestServer(t, addr, "pw", 1)

	start := time.Now()
	code, got := post(t, s, `{"command":"status"}`)
	elapsed := time.Since(start)

	if code != http.StatusGatewayTimeout {
		t.Fatalf("got %d %+v, want 504", code, got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %s, the 1s deadline did not bound the auth stall", elapsed)
	}
	t.Logf("auth stall reported in %s: %s", elapsed.Round(time.Millisecond), got.Error)

	// The abandoned goroutine must drop its upstream socket promptly rather than
	// holding it for another full deadline after the client already got its 504.
	select {
	case held := <-closed:
		if held > 2*time.Second {
			t.Fatalf("upstream socket held %s after a 1s timeout", held)
		}
		t.Logf("upstream socket released after %s", held.Round(time.Millisecond))
	case <-time.After(4 * time.Second):
		t.Fatal("upstream socket was never released")
	}
}

func TestShutdownGraceExceedsRCONTimeout(t *testing.T) {
	// The whole point of deriving it: an in-flight command must always be able to
	// finish or time out on its own terms before the drain gives up.
	for _, timeout := range []time.Duration{time.Second, 5 * time.Second, 60 * time.Second} {
		if grace := shutdownGrace(timeout); grace <= timeout {
			t.Errorf("grace %s does not exceed RCON timeout %s", grace, timeout)
		}
	}
}

// TestConcurrencyCap is the protection for the game server: Source handles RCON
// on the main thread and bans clients that pile on connections, so concurrent
// API requests must not turn into a matching pile of upstream connections.
func TestConcurrencyCap(t *testing.T) {
	const limit = 2
	const callers = 20

	// Count how many connections the upstream sees at once. The handler holds
	// each one open long enough for the burst to overlap.
	var mu sync.Mutex
	live, peak := 0, 0

	srv, err := rcontest.New(rcontest.Respond("pw", 0, func(string) string {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()

		time.Sleep(300 * time.Millisecond)

		mu.Lock()
		live--
		mu.Unlock()

		return "ok"
	}))
	if err != nil {
		t.Fatalf("start fake rcon server: %v", err)
	}
	t.Cleanup(srv.Close)

	host, port := splitAddr(t, srv.Addr())
	s, err := New(&config.Config{
		LogLevel: config.LogLevelInfo,
		HTTP:     config.HTTP{Bind: loopback, Port: 8080},
		RCON:     config.RCON{Host: host, Port: port, Password: "pw", TimeoutSeconds: 5, MaxConcurrent: limit},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	codes := make([]int, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = post(t, s, `{"command":"status"}`)
		}(i)
	}
	wg.Wait()

	mu.Lock()
	gotPeak := peak
	mu.Unlock()

	if gotPeak > limit {
		t.Fatalf("upstream saw %d concurrent connections, cap was %d", gotPeak, limit)
	}

	var ok, busy, other int
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusServiceUnavailable:
			busy++
		default:
			other++
		}
	}
	if other != 0 {
		t.Fatalf("%d requests failed with an unexpected status: %v", other, codes)
	}
	if ok == 0 {
		t.Fatal("every request was shed; the cap should still let work through")
	}
	if busy == 0 {
		t.Fatal("no request was shed, so the cap was never exercised")
	}
	t.Logf("%d callers, cap %d: peak upstream=%d, %d ok, %d shed with 503", callers, limit, gotPeak, ok, busy)
}

// TestBusySlotsAreReleased guards the obvious way to break a semaphore: leaking
// a slot per request until the service permanently returns 503.
func TestBusySlotsAreReleased(t *testing.T) {
	rs := newTestRCON(t)
	host, port := splitAddr(t, rs.Addr())
	s, err := New(&config.Config{
		LogLevel: config.LogLevelInfo,
		HTTP:     config.HTTP{Bind: loopback, Port: 8080},
		RCON:     config.RCON{Host: host, Port: port, Password: "pw", TimeoutSeconds: 5, MaxConcurrent: 1},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// With a cap of 1, serial requests must all succeed.
	for i := range 5 {
		if code, got := post(t, s, `{"command":"status"}`); code != http.StatusOK {
			t.Fatalf("serial request %d got %d %+v; a slot was leaked", i, code, got)
		}
	}
}
