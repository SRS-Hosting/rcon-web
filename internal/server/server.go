// Package server serves the web console and the single API endpoint behind it.
package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/USA-RedDragon/rcon"
	"github.com/USA-RedDragon/rcon-web/internal/config"
)

//go:embed assets/index.html
var assets embed.FS

// shutdownGrace is how long in-flight requests get to finish after a signal.
// It is derived from the RCON timeout rather than fixed, so a command already in
// flight can always run to completion or time out on its own terms: a hardcoded
// grace shorter than the configured timeout would drop the request and exit
// non-zero on every rolling restart.
func shutdownGrace(rconTimeout time.Duration) time.Duration {
	return rconTimeout + 5*time.Second
}

// maxRequestBody caps a command submission. RCON itself rejects anything over
// MaxCommandLen bytes, so this only exists to stop a client from streaming
// megabytes into the decoder.
const maxRequestBody = 4 << 10

// contentTypeJSON is required on command submissions. Enforcing it is what keeps
// the endpoint from being CSRF-able: a cross-origin fetch with this type is not
// a CORS "simple request", so the browser must preflight it, and a plain HTML
// form cannot produce it at all. Without the check, any page on the internet can
// make a victim's browser run RCON commands, because the ingress credential
// rides along automatically.
const contentTypeJSON = "application/json"

// Server owns the HTTP listeners and the RCON client it delegates to.
type Server struct {
	http *http.Server
	rcon *rcon.Client
	page []byte
	bind string
	port int
}

// New builds a Server for cfg. It fails if the embedded page cannot be
// rendered, which would mean a broken build rather than a runtime condition.
func New(cfg *config.Config) (*Server, error) {
	client := rcon.New(cfg.RCON.Addr(), cfg.RCON.Password,
		rcon.WithTimeout(cfg.RCON.Timeout()),
		rcon.WithMaxConcurrent(cfg.RCON.MaxConcurrent))

	page, err := renderPage(client)
	if err != nil {
		return nil, err
	}

	s := &Server{
		rcon: client,
		page: page,
		bind: cfg.HTTP.Bind,
		port: cfg.HTTP.Port,
	}

	mux := http.NewServeMux()
	// "{$}" matches the root path exactly, so anything else falls through to
	// the mux's own 404 instead of being served the page.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /api/command", s.handleCommand)

	s.http = &http.Server{
		Addr:              cfg.HTTP.Addr(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// A response cannot be written until the RCON exchange finishes, so the
		// write deadline has to clear that with room to spare.
		WriteTimeout: cfg.RCON.Timeout() + 10*time.Second,
		IdleTimeout:  60 * time.Second,
		ErrorLog:     slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}

	return s, nil
}

// renderPage renders the console once at startup. The page only depends on
// configuration, which does not change while the process runs.
func renderPage(client *rcon.Client) ([]byte, error) {
	tmpl, err := template.ParseFS(assets, "assets/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse embedded page: %w", err)
	}

	var buf bytes.Buffer
	data := struct {
		Target         string
		TimeoutSeconds int
	}{
		Target:         client.Addr(),
		TimeoutSeconds: int(client.Timeout().Seconds()),
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render embedded page: %w", err)
	}
	return buf.Bytes(), nil
}

// listen opens the listeners for the configured bind address.
//
// A wildcard bind takes one listener per family rather than relying on a single
// dual-stack socket: Go sets IPV6_V6ONLY on a "tcp6" listener, so the two can
// share a port, and IPv4 reachability stops depending on the host's
// net.ipv6.bindv6only setting.
//
// Both families are attempted, but only one has to succeed. A host with a stack
// disabled still gets a working service on the other, with a warning naming what
// was lost; the "listening" lines that follow say exactly which families are up.
// Note that this treats a port already taken on one family the same as a missing
// stack, so it can degrade to half-reachable instead of reporting the conflict.
func (s *Server) listen(ctx context.Context) ([]net.Listener, error) {
	var lc net.ListenConfig
	port := strconv.Itoa(s.port)

	if s.bind != "" && s.bind != "*" {
		// An explicit address already picks its own family, and there is no
		// second one to fall back to.
		addr := net.JoinHostPort(s.bind, port)
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		return []net.Listener{ln}, nil
	}

	families := []struct{ network, host string }{
		{"tcp4", "0.0.0.0"},
		{"tcp6", "::"},
	}
	listeners := make([]net.Listener, 0, len(families))
	errs := make([]error, 0, len(families))

	for _, family := range families {
		ln, err := lc.Listen(ctx, family.network, net.JoinHostPort(family.host, port))
		if err != nil {
			slog.Warn("could not listen on this address family, continuing without it",
				"network", family.network, "port", port, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", family.network, err))
			continue
		}
		listeners = append(listeners, ln)
	}

	if len(listeners) == 0 {
		return nil, fmt.Errorf("listen on port %s: %w", port, errors.Join(errs...))
	}
	return listeners, nil
}

// Run serves until ctx is cancelled, then drains in-flight requests.
func (s *Server) Run(ctx context.Context) error {
	listeners, err := s.listen(ctx)
	if err != nil {
		return err
	}

	// Buffered for every listener so each goroutine can exit even when shutdown
	// wins the race below.
	serveErr := make(chan error, len(listeners))
	for _, ln := range listeners {
		slog.Info("listening", "addr", ln.Addr().String(), "network", ln.Addr().Network(), "rcon", s.rcon.Addr())
		go func(ln net.Listener) {
			err := s.http.Serve(ln)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			serveErr <- err
		}(ln)
	}

	select {
	case err := <-serveErr:
		// Failing to open a family is tolerated above, but a listener that dies
		// after it started accepting is unexpected, so give up entirely and let
		// the supervisor restart us. Close() rather than leaking the sibling
		// listener, which would otherwise keep accepting into a dead process.
		if cerr := s.http.Close(); cerr != nil {
			slog.Debug("close after serve error", "error", cerr)
		}
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	grace := shutdownGrace(s.rcon.Timeout())
	slog.Info("shutting down", "grace", grace)

	// WithoutCancel keeps ctx's values but drops its cancellation: ctx has
	// already fired, and the point of this context is to give open requests time
	// to land rather than to cut them off.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	if err := s.http.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	// Collect from every listener so none is left mid-flight when we return.
	for range listeners {
		if err := <-serveErr; err != nil {
			return fmt.Errorf("serve: %w", err)
		}
	}

	slog.Info("stopped")
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// The page is entirely self-contained, so this policy is both exact and a
	// runtime guarantee of the offline requirement: anything that later reached
	// for a CDN would be blocked rather than silently working in development.
	// frame-ancestors stops the console being framed and clicked through on a
	// victim's authenticated session.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
			"connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if _, err := w.Write(s.page); err != nil {
		slog.Debug("write page", "error", err)
	}
}

type commandRequest struct {
	Command string `json:"command"`
}

type commandResponse struct {
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != contentTypeJSON {
		s.writeJSON(w, http.StatusUnsupportedMediaType,
			commandResponse{Error: "Content-Type must be " + contentTypeJSON})
		return
	}

	var req commandRequest
	limited := http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		// Distinguish "too big" from "not valid JSON": telling someone their JSON
		// is malformed when it was merely truncated sends them hunting a syntax
		// error that is not there.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeJSON(w, http.StatusRequestEntityTooLarge,
				commandResponse{Error: fmt.Sprintf("request body must be at most %d bytes", maxRequestBody)})
			return
		}
		s.writeJSON(w, http.StatusBadRequest, commandResponse{Error: "malformed request body"})
		return
	}
	if req.Command == "" {
		s.writeJSON(w, http.StatusBadRequest, commandResponse{Error: "command must not be empty"})
		return
	}
	output, err := s.rcon.Execute(r.Context(), req.Command)
	if err != nil {
		status, msg := classify(err)
		slog.Warn("command failed", "command", req.Command, "status", status, "error", err)
		s.writeJSON(w, status, commandResponse{Error: msg})
		return
	}

	slog.Info("command executed", "command", req.Command)
	s.writeJSON(w, http.StatusOK, commandResponse{Output: output})
}

// classify maps an RCON failure onto a status code and a message the page can
// show as-is. Everything the operator needs to act on is in the message; the
// full error goes to the log.
func classify(err error) (int, string) {
	var timeout *rcon.TimeoutError
	switch {
	case errors.Is(err, rcon.ErrBusy):
		// 503 rather than 429: the limit protects the game server from concurrent
		// connections, so it is a capacity condition, not a per-client rate.
		return http.StatusServiceUnavailable, "too many commands in flight, try again in a moment"
	case errors.As(err, &timeout):
		return http.StatusGatewayTimeout, timeout.Error()
	case errors.Is(err, rcon.ErrAuthFailed):
		return http.StatusBadGateway, "rcon: authentication failed - check the configured password"
	case errors.Is(err, rcon.ErrNotRCON):
		return http.StatusBadGateway, "rcon: the configured address is not an RCON server"
	case errors.Is(err, rcon.ErrCommandTooLong):
		// The client rejects this before dialling, so it is a 400 about the
		// request rather than a 502 about the upstream.
		return http.StatusBadRequest, fmt.Sprintf("command must be at most %d bytes", rcon.MaxCommandLen)
	default:
		return http.StatusBadGateway, err.Error()
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body commandResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("write response", "error", err)
	}
}
