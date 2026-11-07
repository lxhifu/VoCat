package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxConfigBytes = 1 << 20

// Config contains the process-level settings shared by the HTTP and storage
// layers. Environment variables override values loaded from VOCAT_CONFIG.
type Config struct {
	Address             string
	DatabasePath        string
	SessionTTL          time.Duration
	SecureCookies       bool
	ShutdownTimeout     time.Duration
	MaxRequestBodyBytes int64
	MCPEnabled          bool
	ProbeUnknownUSB     bool
	// Modem crash recovery watcher. Disabled by default: when enabled, vocat
	// polls the modem's remoteproc and writes "recover" after a confirmed
	// firmware crash (see internal/remoteproc). It never changes the
	// remoteproc recovery policy setting itself.
	ModemRecoveryEnabled       bool
	ModemRecoveryPollInterval  time.Duration
	ModemRecoverySettle        time.Duration
	ModemRecoveryMaxAttempts   int
	ModemRecoveryAttemptWindow time.Duration
	ModemRecoveryCooldown      time.Duration
}

type fileConfig struct {
	Address      *string `json:"address"`
	DatabasePath *string `json:"database_path"`
	// Retain the legacy keys only so upgrades do not reject an existing config
	// file. They are deliberately ignored: administrator credentials are read
	// exclusively from SQLite.
	LegacyAdminUsername        *string `json:"admin_username"`
	LegacyAdminPassword        *string `json:"admin_password"`
	SessionTTL                 *string `json:"session_ttl"`
	SecureCookies              *bool   `json:"secure_cookies"`
	ShutdownTimeout            *string `json:"shutdown_timeout"`
	MaxRequestBodyBytes        *int64  `json:"max_request_body_bytes"`
	MCPEnabled                 *bool   `json:"mcp_enabled"`
	ProbeUnknownUSB            *bool   `json:"probe_unknown_usb"`
	ModemRecoveryEnabled       *bool   `json:"modem_recovery_enabled"`
	ModemRecoveryPollInterval  *string `json:"modem_recovery_poll_interval"`
	ModemRecoverySettle        *string `json:"modem_recovery_settle"`
	ModemRecoveryMaxAttempts   *int    `json:"modem_recovery_max_attempts"`
	ModemRecoveryAttemptWindow *string `json:"modem_recovery_attempt_window"`
	ModemRecoveryCooldown      *string `json:"modem_recovery_cooldown"`
}

// Default returns the non-secret process configuration. Administrator
// credentials are initialized separately and stored only in SQLite.
func Default() Config {
	return Config{
		Address:             "0.0.0.0:7575",
		DatabasePath:        "./data/vocat.db",
		SessionTTL:          24 * time.Hour,
		SecureCookies:       false,
		ShutdownTimeout:     10 * time.Second,
		MaxRequestBodyBytes: 1 << 20,
		// The MCP endpoint is authenticated by a bearer token and mounted
		// by default; operators who do not want it set VOCAT_MCP_ENABLED=false.
		MCPEnabled: true,
		// The recovery watcher stays off until an operator opts in: writing
		// to sysfs also requires the systemd unit to grant write access to
		// /sys/class/remoteproc (see deploy/vocat.service).
		ModemRecoveryEnabled:       false,
		ModemRecoveryPollInterval:  10 * time.Second,
		ModemRecoverySettle:        90 * time.Second,
		ModemRecoveryMaxAttempts:   3,
		ModemRecoveryAttemptWindow: time.Hour,
		ModemRecoveryCooldown:      5 * time.Minute,
	}
}

// Load reads an optional strict JSON file selected by VOCAT_CONFIG and then
// applies VOCAT_* environment overrides.
func Load() (Config, error) {
	cfg := Default()

	if path := strings.TrimSpace(os.Getenv("VOCAT_CONFIG")); path != "" {
		fileValues, err := loadFile(path)
		if err != nil {
			return Config{}, err
		}
		if err := applyFile(&cfg, fileValues); err != nil {
			return Config{}, fmt.Errorf("load config %q: %w", path, err)
		}
	}

	if err := applyEnvironment(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadFile(path string) (fileConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return fileConfig{}, fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileConfig{}, fmt.Errorf("stat config %q: %w", path, err)
	}
	if info.Size() > maxConfigBytes {
		return fileConfig{}, fmt.Errorf("config %q exceeds %d bytes", path, maxConfigBytes)
	}

	decoder := json.NewDecoder(io.LimitReader(file, maxConfigBytes))
	decoder.DisallowUnknownFields()

	var values fileConfig
	if err := decoder.Decode(&values); err != nil {
		return fileConfig{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fileConfig{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	return values, nil
}

func applyFile(cfg *Config, values fileConfig) error {
	if values.Address != nil {
		cfg.Address = *values.Address
	}
	if values.DatabasePath != nil {
		cfg.DatabasePath = *values.DatabasePath
	}
	if values.SessionTTL != nil {
		duration, err := time.ParseDuration(*values.SessionTTL)
		if err != nil {
			return fmt.Errorf("session_ttl: %w", err)
		}
		cfg.SessionTTL = duration
	}
	if values.SecureCookies != nil {
		cfg.SecureCookies = *values.SecureCookies
	}
	if values.ShutdownTimeout != nil {
		duration, err := time.ParseDuration(*values.ShutdownTimeout)
		if err != nil {
			return fmt.Errorf("shutdown_timeout: %w", err)
		}
		cfg.ShutdownTimeout = duration
	}
	if values.MaxRequestBodyBytes != nil {
		cfg.MaxRequestBodyBytes = *values.MaxRequestBodyBytes
	}
	if values.MCPEnabled != nil {
		cfg.MCPEnabled = *values.MCPEnabled
	}
	if values.ProbeUnknownUSB != nil {
		cfg.ProbeUnknownUSB = *values.ProbeUnknownUSB
	}
	if values.ModemRecoveryEnabled != nil {
		cfg.ModemRecoveryEnabled = *values.ModemRecoveryEnabled
	}
	if values.ModemRecoveryPollInterval != nil {
		duration, err := time.ParseDuration(*values.ModemRecoveryPollInterval)
		if err != nil {
			return fmt.Errorf("modem_recovery_poll_interval: %w", err)
		}
		cfg.ModemRecoveryPollInterval = duration
	}
	if values.ModemRecoverySettle != nil {
		duration, err := time.ParseDuration(*values.ModemRecoverySettle)
		if err != nil {
			return fmt.Errorf("modem_recovery_settle: %w", err)
		}
		cfg.ModemRecoverySettle = duration
	}
	if values.ModemRecoveryMaxAttempts != nil {
		cfg.ModemRecoveryMaxAttempts = *values.ModemRecoveryMaxAttempts
	}
	if values.ModemRecoveryAttemptWindow != nil {
		duration, err := time.ParseDuration(*values.ModemRecoveryAttemptWindow)
		if err != nil {
			return fmt.Errorf("modem_recovery_attempt_window: %w", err)
		}
		cfg.ModemRecoveryAttemptWindow = duration
	}
	if values.ModemRecoveryCooldown != nil {
		duration, err := time.ParseDuration(*values.ModemRecoveryCooldown)
		if err != nil {
			return fmt.Errorf("modem_recovery_cooldown: %w", err)
		}
		cfg.ModemRecoveryCooldown = duration
	}
	return nil
}

func applyEnvironment(cfg *Config) error {
	applyString := func(name string, target *string) {
		if value, ok := os.LookupEnv(name); ok {
			*target = value
		}
	}

	applyString("VOCAT_ADDR", &cfg.Address)
	applyString("VOCAT_DATABASE_PATH", &cfg.DatabasePath)

	if value, ok := os.LookupEnv("VOCAT_SESSION_TTL"); ok {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("VOCAT_SESSION_TTL: %w", err)
		}
		cfg.SessionTTL = duration
	}
	if value, ok := os.LookupEnv("VOCAT_SECURE_COOKIES"); ok {
		secure, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("VOCAT_SECURE_COOKIES: %w", err)
		}
		cfg.SecureCookies = secure
	}
	if value, ok := os.LookupEnv("VOCAT_SHUTDOWN_TIMEOUT"); ok {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("VOCAT_SHUTDOWN_TIMEOUT: %w", err)
		}
		cfg.ShutdownTimeout = duration
	}
	if value, ok := os.LookupEnv("VOCAT_MAX_REQUEST_BODY_BYTES"); ok {
		size, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("VOCAT_MAX_REQUEST_BODY_BYTES: %w", err)
		}
		cfg.MaxRequestBodyBytes = size
	}
	if value, ok := os.LookupEnv("VOCAT_MCP_ENABLED"); ok {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("VOCAT_MCP_ENABLED: %w", err)
		}
		cfg.MCPEnabled = enabled
	}
	if value, ok := os.LookupEnv("VOCAT_PROBE_UNKNOWN_USB"); ok {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("VOCAT_PROBE_UNKNOWN_USB: %w", err)
		}
		cfg.ProbeUnknownUSB = enabled
	}
	if value, ok := os.LookupEnv("VOCAT_MODEM_RECOVERY_ENABLED"); ok {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("VOCAT_MODEM_RECOVERY_ENABLED: %w", err)
		}
		cfg.ModemRecoveryEnabled = enabled
	}
	if value, ok := os.LookupEnv("VOCAT_MODEM_RECOVERY_POLL_INTERVAL"); ok {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("VOCAT_MODEM_RECOVERY_POLL_INTERVAL: %w", err)
		}
		cfg.ModemRecoveryPollInterval = duration
	}
	if value, ok := os.LookupEnv("VOCAT_MODEM_RECOVERY_SETTLE"); ok {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("VOCAT_MODEM_RECOVERY_SETTLE: %w", err)
		}
		cfg.ModemRecoverySettle = duration
	}
	if value, ok := os.LookupEnv("VOCAT_MODEM_RECOVERY_MAX_ATTEMPTS"); ok {
		attempts, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("VOCAT_MODEM_RECOVERY_MAX_ATTEMPTS: %w", err)
		}
		cfg.ModemRecoveryMaxAttempts = attempts
	}
	if value, ok := os.LookupEnv("VOCAT_MODEM_RECOVERY_ATTEMPT_WINDOW"); ok {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("VOCAT_MODEM_RECOVERY_ATTEMPT_WINDOW: %w", err)
		}
		cfg.ModemRecoveryAttemptWindow = duration
	}
	if value, ok := os.LookupEnv("VOCAT_MODEM_RECOVERY_COOLDOWN"); ok {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("VOCAT_MODEM_RECOVERY_COOLDOWN: %w", err)
		}
		cfg.ModemRecoveryCooldown = duration
	}
	return nil
}

// Validate rejects settings that would make the server unusable or weaken its
// basic request limits.
func (cfg Config) Validate() error {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(cfg.Address))
	if err != nil {
		return fmt.Errorf("address: %w", err)
	}
	_ = host
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("address: invalid TCP port %q", portText)
	}
	if strings.TrimSpace(cfg.DatabasePath) == "" {
		return errors.New("database_path must not be empty")
	}
	if cfg.SessionTTL < 5*time.Minute || cfg.SessionTTL > 30*24*time.Hour {
		return errors.New("session_ttl must be between 5m and 720h")
	}
	if cfg.ShutdownTimeout <= 0 || cfg.ShutdownTimeout > 5*time.Minute {
		return errors.New("shutdown_timeout must be between 1ns and 5m")
	}
	if cfg.MaxRequestBodyBytes < 1024 || cfg.MaxRequestBodyBytes > 10<<20 {
		return errors.New("max_request_body_bytes must be between 1024 and 10485760")
	}
	if cfg.ModemRecoveryPollInterval < time.Second || cfg.ModemRecoveryPollInterval > 5*time.Minute {
		return errors.New("modem_recovery_poll_interval must be between 1s and 5m")
	}
	if cfg.ModemRecoverySettle < 0 || cfg.ModemRecoverySettle > 10*time.Minute {
		return errors.New("modem_recovery_settle must be between 0s and 10m")
	}
	if cfg.ModemRecoveryMaxAttempts < 1 || cfg.ModemRecoveryMaxAttempts > 10 {
		return errors.New("modem_recovery_max_attempts must be between 1 and 10")
	}
	if cfg.ModemRecoveryAttemptWindow < time.Minute || cfg.ModemRecoveryAttemptWindow > 24*time.Hour {
		return errors.New("modem_recovery_attempt_window must be between 1m and 24h")
	}
	if cfg.ModemRecoveryCooldown < 0 || cfg.ModemRecoveryCooldown > cfg.ModemRecoveryAttemptWindow {
		return errors.New("modem_recovery_cooldown must be between 0s and modem_recovery_attempt_window")
	}
	return nil
}
