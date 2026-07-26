package config

import (
	"strings"
	"testing"
	"time"
)

func valid() Config {
	return Config{
		LogLevel: LogLevelInfo,
		HTTP:     HTTP{Bind: "", Port: 8080},
		RCON:     RCON{Host: "127.0.0.1", Port: 27015, Password: "secret", TimeoutSeconds: 5, MaxConcurrent: 4},
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// TestValidateRangeChecks covers the values a YAML file can carry that a narrower
// field type would silently wrap: 70000 truncating to 4464 and -1 to 65535 were
// both real, and neither is visible to a != 0 check.
func TestValidateRangeChecks(t *testing.T) {
	tests := []struct {
		name  string
		mutef func(*Config)
		want  string
	}{
		{"port too high", func(c *Config) { c.HTTP.Port = 70000 }, "http.port"},
		{"port negative", func(c *Config) { c.HTTP.Port = -1 }, "http.port"},
		{"port zero", func(c *Config) { c.HTTP.Port = 0 }, "http.port"},
		{"rcon port too high", func(c *Config) { c.RCON.Port = 65536 }, "rcon.port"},
		{"rcon port negative", func(c *Config) { c.RCON.Port = -8080 }, "rcon.port"},
		{"timeout zero", func(c *Config) { c.RCON.TimeoutSeconds = 0 }, "rcon.timeoutSeconds"},
		{"timeout negative", func(c *Config) { c.RCON.TimeoutSeconds = -1 }, "rcon.timeoutSeconds"},
		{"timeout too high", func(c *Config) { c.RCON.TimeoutSeconds = 65537 }, "rcon.timeoutSeconds"},
		{"no password", func(c *Config) { c.RCON.Password = "" }, "rcon.password"},
		{"no host", func(c *Config) { c.RCON.Host = "" }, "rcon.host"},
		{"bad log level", func(c *Config) { c.LogLevel = "silly" }, "logLevel"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutef(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestValidateReportsEveryProblem is the documented behaviour: a bad deployment
// should not have to be fixed one restart at a time.
func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Config{} // every field wrong at once
	err := cfg.Validate()
	if err == nil {
		t.Fatal("empty config was accepted")
	}
	for _, want := range []string{"logLevel", "http.port", "rcon.host", "rcon.port", "rcon.password", "rcon.timeoutSeconds"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestAddrAndTimeout(t *testing.T) {
	cfg := valid()

	if got := cfg.RCON.Addr(); got != "127.0.0.1:27015" {
		t.Errorf("RCON.Addr() = %q", got)
	}
	if got := cfg.RCON.Timeout(); got != 5*time.Second {
		t.Errorf("RCON.Timeout() = %s", got)
	}
	// An empty bind is the dual-stack default and must still form a valid address.
	if got := cfg.HTTP.Addr(); got != ":8080" {
		t.Errorf("HTTP.Addr() with empty bind = %q", got)
	}

	cfg.HTTP.Bind = "::1"
	if got := cfg.HTTP.Addr(); got != "[::1]:8080" {
		t.Errorf("HTTP.Addr() with IPv6 bind = %q, want brackets", got)
	}
}

// TestMaxTimeoutIsSane guards the coupling to the server's shutdown grace: the
// cap exists so a rolling restart cannot be held open arbitrarily long.
func TestMaxTimeoutIsSane(t *testing.T) {
	if MaxTimeoutSeconds < 1 || MaxTimeoutSeconds > 300 {
		t.Fatalf("MaxTimeoutSeconds = %d, outside a defensible range", MaxTimeoutSeconds)
	}
	cfg := valid()
	cfg.RCON.TimeoutSeconds = MaxTimeoutSeconds
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the cap itself was rejected: %v", err)
	}
}
