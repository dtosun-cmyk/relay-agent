package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config represents the application configuration
type Config struct {
	Server struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
	} `yaml:"server"`

	MongoDB struct {
		URI      string `yaml:"uri"`
		Database string `yaml:"database"`
	} `yaml:"mongodb"`

	Mailgateway struct {
		RelayServerID int `yaml:"relay_server_id"`
	} `yaml:"mailgateway"`

	Postfix struct {
		LogFile string `yaml:"log_file"`
	} `yaml:"postfix"`

	SMTP struct {
		Domain    string `yaml:"domain"`     // SASL domain for SMTP authentication
		APISecret string `yaml:"api_secret"` // Shared secret for API authentication
	} `yaml:"smtp"`

	Filter struct {
		Enabled    bool   `yaml:"enabled"`     // Enable SMTP content filter
		ListenAddr string `yaml:"listen_addr"` // Address to listen on (e.g., 127.0.0.1:10025)
		NextHop    string `yaml:"next_hop"`    // Next hop address (e.g., 127.0.0.1:10026)
		Hostname   string `yaml:"hostname"`    // Hostname for SMTP greeting
	} `yaml:"filter"`

	Processing struct {
		BatchSize     int `yaml:"batch_size"`     // MongoDB batch insert size
		FlushInterval int `yaml:"flush_interval"` // seconds
		ChannelBuffer int `yaml:"channel_buffer"` // log channel buffer size
	} `yaml:"processing"`

	Logging struct {
		Level string `yaml:"level"`
		File  string `yaml:"file"`
	} `yaml:"logging"`
}

var (
	ErrConfigNotFound   = errors.New("config file not found")
	ErrInvalidConfig    = errors.New("invalid configuration")
	ErrValidationFailed = errors.New("validation failed")
)

// Load reads and parses the configuration file from the given path.
// It also applies environment variable overrides after loading.
// Returns a validated Config or an error.
func Load(path string) (*Config, error) {
	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrConfigNotFound, path)
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse YAML
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	// Apply environment variable overrides
	applyEnvOverrides(&cfg)

	// Validate
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyEnvOverrides applies environment variable overrides to the configuration.
// Environment variables follow the pattern: RELAY_SECTION_FIELD (e.g., RELAY_SERVER_HOST)
// This function is designed to minimize allocations by using byte operations and string builder where needed.
func applyEnvOverrides(cfg *Config) {
	// Server section
	if val := os.Getenv("RELAY_SERVER_HOST"); val != "" {
		cfg.Server.Host = val
	}
	if val := os.Getenv("RELAY_SERVER_PORT"); val != "" {
		if port, err := strconv.Atoi(val); err == nil {
			cfg.Server.Port = port
		}
	}

	// MongoDB section
	if val := os.Getenv("RELAY_MONGODB_URI"); val != "" {
		cfg.MongoDB.URI = val
	}
	if val := os.Getenv("RELAY_MONGODB_DATABASE"); val != "" {
		cfg.MongoDB.Database = val
	}

	// Mailgateway section
	if val := os.Getenv("RELAY_MAILGATEWAY_RELAY_SERVER_ID"); val != "" {
		if id, err := strconv.Atoi(val); err == nil {
			cfg.Mailgateway.RelayServerID = id
		}
	}

	// Postfix section
	if val := os.Getenv("RELAY_POSTFIX_LOG_FILE"); val != "" {
		cfg.Postfix.LogFile = val
	}

	// SMTP section
	if val := os.Getenv("RELAY_SMTP_DOMAIN"); val != "" {
		cfg.SMTP.Domain = val
	}
	if val := os.Getenv("RELAY_SMTP_API_SECRET"); val != "" {
		cfg.SMTP.APISecret = val
	}

	// Filter section
	if val := os.Getenv("RELAY_FILTER_ENABLED"); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			cfg.Filter.Enabled = enabled
		}
	}
	if val := os.Getenv("RELAY_FILTER_LISTEN_ADDR"); val != "" {
		cfg.Filter.ListenAddr = val
	}
	if val := os.Getenv("RELAY_FILTER_NEXT_HOP"); val != "" {
		cfg.Filter.NextHop = val
	}
	if val := os.Getenv("RELAY_FILTER_HOSTNAME"); val != "" {
		cfg.Filter.Hostname = val
	}

	// Processing section
	if val := os.Getenv("RELAY_PROCESSING_BATCH_SIZE"); val != "" {
		if size, err := strconv.Atoi(val); err == nil {
			cfg.Processing.BatchSize = size
		}
	}
	if val := os.Getenv("RELAY_PROCESSING_FLUSH_INTERVAL"); val != "" {
		if interval, err := strconv.Atoi(val); err == nil {
			cfg.Processing.FlushInterval = interval
		}
	}
	if val := os.Getenv("RELAY_PROCESSING_CHANNEL_BUFFER"); val != "" {
		if buffer, err := strconv.Atoi(val); err == nil {
			cfg.Processing.ChannelBuffer = buffer
		}
	}

	// Logging section
	if val := os.Getenv("RELAY_LOGGING_LEVEL"); val != "" {
		cfg.Logging.Level = val
	}
	if val := os.Getenv("RELAY_LOGGING_FILE"); val != "" {
		cfg.Logging.File = val
	}
}

// Validate checks if the configuration is valid.
// It returns a detailed error describing what validation failed.
func (c *Config) Validate() error {
	var errs []string

	// Server validation
	if c.Server.Host == "" {
		errs = append(errs, "server.host is required")
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		errs = append(errs, "server.port must be between 1 and 65535")
	}

	// MongoDB validation
	if c.MongoDB.URI == "" {
		errs = append(errs, "mongodb.uri is required")
	}
	if c.MongoDB.Database == "" {
		errs = append(errs, "mongodb.database is required")
	}

	// Mailgateway validation
	if c.Mailgateway.RelayServerID <= 0 {
		errs = append(errs, "mailgateway.relay_server_id must be greater than 0")
	}

	// Postfix validation
	if c.Postfix.LogFile == "" {
		errs = append(errs, "postfix.log_file is required")
	}

	// SMTP validation
	if c.SMTP.Domain == "" {
		errs = append(errs, "smtp.domain is required")
	}
	if c.SMTP.APISecret == "" {
		errs = append(errs, "smtp.api_secret is required")
	}
	if len(c.SMTP.APISecret) < 16 {
		errs = append(errs, "smtp.api_secret must be at least 16 characters for security")
	}

	// Processing validation
	if c.Processing.BatchSize <= 0 {
		errs = append(errs, "processing.batch_size must be greater than 0")
	}
	if c.Processing.FlushInterval <= 0 {
		errs = append(errs, "processing.flush_interval must be greater than 0")
	}
	if c.Processing.ChannelBuffer < 0 {
		errs = append(errs, "processing.channel_buffer must be greater than or equal to 0")
	}

	// Logging validation
	if c.Logging.Level == "" {
		errs = append(errs, "logging.level is required")
	} else {
		// Validate log level (zero allocation check using switch)
		level := strings.ToLower(c.Logging.Level)
		switch level {
		case "debug", "info", "warn", "warning", "error", "fatal", "panic":
			// Valid log level
		default:
			errs = append(errs, "logging.level must be one of: debug, info, warn, error, fatal, panic")
		}
	}
	if c.Logging.File == "" {
		errs = append(errs, "logging.file is required")
	}

	if len(errs) > 0 {
		// Build error message efficiently
		var sb strings.Builder
		sb.WriteString("configuration validation failed:\n")
		for i, err := range errs {
			if i > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString("  - ")
			sb.WriteString(err)
		}
		return fmt.Errorf("%w: %s", ErrValidationFailed, sb.String())
	}

	return nil
}

// SetDefaults applies reasonable default values to the configuration.
// This is useful when creating a new config or for testing.
func (c *Config) SetDefaults() {
	if c.Server.Host == "" {
		c.Server.Host = "0.0.0.0"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}

	if c.Processing.BatchSize == 0 {
		c.Processing.BatchSize = 100
	}
	if c.Processing.FlushInterval == 0 {
		c.Processing.FlushInterval = 5
	}
	if c.Processing.ChannelBuffer == 0 {
		c.Processing.ChannelBuffer = 1000
	}

	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.File == "" {
		c.Logging.File = "/var/log/relay-agent/relay-agent.log"
	}

	if c.Postfix.LogFile == "" {
		c.Postfix.LogFile = "/var/log/mail.log"
	}

	if c.Filter.ListenAddr == "" {
		c.Filter.ListenAddr = "127.0.0.1:10025"
	}
	if c.Filter.NextHop == "" {
		c.Filter.NextHop = "127.0.0.1:10026"
	}
	if c.Filter.Hostname == "" {
		c.Filter.Hostname = "localhost"
	}
}
