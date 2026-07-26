// Package rcon wraps github.com/gorcon/rcon with a context-aware, bounded
// Execute so a dead or unresponsive server produces an error instead of a
// request that never finishes.
package rcon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/gorcon/rcon"
)

// Client executes commands against a Source RCON server.
//
// A fresh connection is opened per command. RCON connections do not survive a
// server restart and there is no state worth keeping warm, so reconnecting each
// time removes a whole class of stale-socket failures for the cost of one TCP
// handshake and auth round trip.
type Client struct {
	addr     string
	password string
	timeout  time.Duration
	// slots bounds how many exchanges may be in flight at once. Source servers
	// are effectively single-threaded and cap or ban RCON clients that pile on
	// connections, so without this a handful of concurrent requests through the
	// API turns into a matching pile of connections against the game server.
	slots chan struct{}
}

// New returns a Client that talks to the RCON server at addr, allowing timeout
// for each complete command exchange and at most maxConcurrent exchanges at a
// time. Callers beyond that limit get ErrBusy rather than being queued: waiting
// would spend the caller's deadline on the queue and then hand it a truncated
// budget for the exchange itself.
func New(addr, password string, timeout time.Duration, maxConcurrent int) *Client {
	// A zero-capacity channel would report ErrBusy for every command instead of
	// meaning "unlimited", so refuse to build that. Validate rejects it in the
	// configured path; this covers anyone constructing a Client directly.
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Client{
		addr:     addr,
		password: password,
		timeout:  timeout,
		slots:    make(chan struct{}, maxConcurrent),
	}
}

// ErrBusy reports that every concurrent-command slot was taken. It is a
// backpressure signal, not a failure of the RCON server.
var ErrBusy = errors.New("rcon: too many commands already in flight")

// Addr returns the address of the RCON server.
func (c *Client) Addr() string { return c.addr }

// Timeout returns the per-command deadline.
func (c *Client) Timeout() time.Duration { return c.timeout }

// TimeoutError reports that an RCON exchange did not finish within the
// per-command deadline. Callers use errors.As to tell a slow or dead server
// apart from a server that answered with a failure.
type TimeoutError struct {
	Addr    string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("rcon: %s did not respond within %s", e.Addr, e.Timeout)
}

type result struct {
	body string
	err  error
}

// Execute runs command on the RCON server and returns its response.
//
// The entire exchange, connect, authenticate, send, receive, shares a single
// deadline, the smaller of ctx and the configured timeout. gorcon's own dial
// timeout and socket deadlines are not enough on their own: each step re-arms the
// full deadline, so a server that answers every step just inside it outlasts the
// budget in aggregate. The select below bounds what the caller waits for, and
// closing the connection on timeout bounds how long the abandoned goroutine can
// hold a socket open against the game server, which would otherwise linger for
// roughly another full timeout after the caller already gave up.
func (c *Client) Execute(ctx context.Context, command string) (string, error) {
	// Claimed before any work starts, and released by the goroutine below rather
	// than on return from here, so the count tracks live upstream connections
	// rather than callers still waiting on one.
	select {
	case c.slots <- struct{}{}:
	default:
		return "", ErrBusy
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Buffered so the goroutine can always deliver and exit, even once we have
	// stopped listening.
	ch := make(chan result, 1)
	// Capacity 1 and a single send: the goroutine publishes the live connection
	// so a timeout here can close it. Closing a net.Conn concurrently with a
	// blocked read is safe and unblocks that read immediately.
	connCh := make(chan io.Closer, 1)

	go func() {
		defer func() { <-c.slots }()
		body, err := c.execute(command, connCh)
		ch <- result{body: body, err: err}
	}()

	select {
	case r := <-ch:
		return r.body, r.err
	case <-ctx.Done():
		select {
		case conn := <-connCh:
			if err := conn.Close(); err != nil {
				slog.Debug("close abandoned rcon connection", "addr", c.addr, "error", err)
			}
		default:
			// Still dialing; gorcon's dial timeout bounds that on its own.
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", &TimeoutError{Addr: c.addr, Timeout: c.timeout}
		}
		return "", fmt.Errorf("rcon: %s: %w", c.addr, ctx.Err())
	}
}

// execute performs the blocking exchange. It is only ever called from the
// goroutine started by Execute, and publishes its connection to connCh so that
// Execute can close it out from under this goroutine on timeout.
func (c *Client) execute(command string, connCh chan<- io.Closer) (string, error) {
	conn, err := rcon.Dial(c.addr, c.password,
		rcon.SetDialTimeout(c.timeout),
		rcon.SetDeadline(c.timeout),
	)
	if err != nil {
		if isTimeout(err) {
			return "", c.timedOut()
		}
		return "", fmt.Errorf("connect to %s: %w", c.addr, err)
	}
	// Dial already authenticated, so publishing here cannot shorten the auth
	// step; the dial timeout covers that. A double Close is harmless.
	connCh <- conn
	defer func() {
		if cerr := conn.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			slog.Debug("close rcon connection", "addr", c.addr, "error", cerr)
		}
	}()

	body, err := conn.Execute(command)
	if err != nil {
		if isTimeout(err) || errors.Is(err, net.ErrClosed) {
			return "", c.timedOut()
		}
		return "", fmt.Errorf("execute %q: %w", command, err)
	}
	return body, nil
}

func (c *Client) timedOut() *TimeoutError {
	return &TimeoutError{Addr: c.addr, Timeout: c.timeout}
}

// isTimeout reports whether err is a socket deadline firing. gorcon's own dial
// timeout and read/write deadlines can trip just before the context does, and
// they mean the same thing to a caller, so they are reported the same way.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
