package config

//go:generate go tool configulator -type Config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// LogLevel selects the verbosity of the structured logger.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// Config is the root configuration.
type Config struct {
	LogLevel LogLevel `name:"logLevel" default:"info" description:"log verbosity: debug, info, warn, or error"`
	HTTP     HTTP     `name:"http" description:"web interface settings"`
	RCON     RCON     `name:"rcon" description:"Source RCON server settings"`
}

// MaxTimeoutSeconds bounds rcon.timeoutSeconds. The page is meant to report a
// dead server quickly, and the shutdown grace is derived from this, so an
// unbounded value would make a rolling restart arbitrarily slow.
const MaxTimeoutSeconds = 60

// MaxConcurrentLimit bounds rcon.maxConcurrent. Anything near this is already
// far past what a Source server tolerates; the ceiling exists to catch a typo,
// not to describe a workable setting.
const MaxConcurrentLimit = 64

const maxPort = 65535

// HTTP configures the listener for the web interface.
type HTTP struct {
	Bind string `name:"bind" description:"address to listen on; empty listens on all interfaces over both IPv4 and IPv6"`
	Port int    `name:"port" default:"8080" description:"port the web interface listens on"`
}

// Addr returns the listen address in host:port form.
func (h HTTP) Addr() string {
	return net.JoinHostPort(h.Bind, strconv.Itoa(h.Port))
}

// RCON configures the upstream Source RCON server.
type RCON struct {
	Host           string `name:"host" default:"127.0.0.1" description:"hostname or IP of the Source RCON server"`
	Port           int    `name:"port" default:"27015" description:"TCP port of the Source RCON server"`
	Password       string `name:"password" secret:"true" description:"RCON password (required)"`
	TimeoutSeconds int    `name:"timeoutSeconds" default:"5" description:"deadline in seconds covering a whole RCON exchange: connect, authenticate, command, response"`
	// Source servers handle RCON on the main thread and ban clients that pile on
	// connections, so the useful value here is small. The page only ever has one
	// command in flight per tab; this is headroom for a few admins at once, not a
	// throughput knob.
	MaxConcurrent int `name:"maxConcurrent" default:"4" description:"maximum RCON commands in flight at once; further requests get HTTP 503"`
}

// Addr returns the RCON server address in host:port form.
func (r RCON) Addr() string {
	return net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
}

// Timeout returns the per-command deadline.
func (r RCON) Timeout() time.Duration {
	return time.Duration(r.TimeoutSeconds) * time.Second
}

// Validate reports every problem with the configuration at once, so a bad
// deployment does not have to be fixed one restart at a time.
func (c Config) Validate() error {
	var errs []error

	switch c.LogLevel {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		errs = append(errs, fmt.Errorf("logLevel %q must be one of debug, info, warn, error", c.LogLevel))
	}
	if c.HTTP.Port < 1 || c.HTTP.Port > maxPort {
		errs = append(errs, fmt.Errorf("http.port %d must be between 1 and %d", c.HTTP.Port, maxPort))
	}
	if c.RCON.Host == "" {
		errs = append(errs, errors.New("rcon.host must not be empty"))
	}
	if c.RCON.Port < 1 || c.RCON.Port > maxPort {
		errs = append(errs, fmt.Errorf("rcon.port %d must be between 1 and %d", c.RCON.Port, maxPort))
	}
	if c.RCON.Password == "" {
		errs = append(errs, errors.New("rcon.password must not be empty"))
	}
	if c.RCON.TimeoutSeconds < 1 || c.RCON.TimeoutSeconds > MaxTimeoutSeconds {
		errs = append(errs, fmt.Errorf("rcon.timeoutSeconds %d must be between 1 and %d",
			c.RCON.TimeoutSeconds, MaxTimeoutSeconds))
	}
	if c.RCON.MaxConcurrent < 1 || c.RCON.MaxConcurrent > MaxConcurrentLimit {
		errs = append(errs, fmt.Errorf("rcon.maxConcurrent %d must be between 1 and %d",
			c.RCON.MaxConcurrent, MaxConcurrentLimit))
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}
