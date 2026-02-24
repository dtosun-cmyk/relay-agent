package smtp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

var (
	// ErrInvalidUsername is returned when username validation fails
	ErrInvalidUsername = errors.New("invalid username: must contain only alphanumeric characters and underscores")

	// ErrInvalidPassword is returned when password validation fails
	ErrInvalidPassword = errors.New("invalid password: must be at least 8 characters")

	// ErrUserExists is returned when attempting to create a user that already exists
	ErrUserExists = errors.New("user already exists")

	// ErrUserNotFound is returned when attempting to delete a user that doesn't exist
	ErrUserNotFound = errors.New("user not found")

	// ErrCommandFailed is returned when a sasl command fails
	ErrCommandFailed = errors.New("sasl command failed")
)

// usernameRegex ensures usernames are safe (alphanumeric + underscore only)
// This prevents shell injection attacks
var usernameRegex = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// UserManager manages SMTP SASL users using saslpasswd2
type UserManager struct {
	domain     string // SASL domain (e.g., smtp.example.com)
	sasldbPath string // Path to sasldb2 file (e.g., /etc/sasldb2)
	logger     zerolog.Logger
}

// User represents a SASL user
type User struct {
	Username  string `json:"username"`
	Domain    string `json:"domain"`
	CreatedAt string `json:"created_at,omitempty"`
}

// CreateUserRequest represents the request to create a new user
type CreateUserRequest struct {
	Username string `json:"username" validate:"required"`
	Password string `json:"password" validate:"required,min=8"`
}

// NewUserManager creates a new UserManager instance
func NewUserManager(domain string, logger zerolog.Logger) *UserManager {
	return &UserManager{
		domain:     domain,
		sasldbPath: "/etc/sasldb2",
		logger:     logger.With().Str("component", "smtp-users").Logger(),
	}
}

// validateUsername checks if the username contains only safe characters
// This is critical for preventing shell injection attacks
func validateUsername(username string) error {
	if username == "" {
		return fmt.Errorf("%w: username cannot be empty", ErrInvalidUsername)
	}

	if len(username) > 64 {
		return fmt.Errorf("%w: username too long (max 64 characters)", ErrInvalidUsername)
	}

	if !usernameRegex.MatchString(username) {
		return ErrInvalidUsername
	}

	return nil
}

// validatePassword checks if the password meets minimum requirements
func validatePassword(password string) error {
	if len(password) < 8 {
		return ErrInvalidPassword
	}

	if len(password) > 256 {
		return fmt.Errorf("%w: password too long (max 256 characters)", ErrInvalidPassword)
	}

	return nil
}

// CreateUser creates a new SASL user with the given username and password
// It uses saslpasswd2 to create the user in the SASL database
func (m *UserManager) CreateUser(username, password string) error {
	// Validate inputs to prevent injection attacks
	if err := validateUsername(username); err != nil {
		return err
	}

	if err := validatePassword(password); err != nil {
		return err
	}

	// Check if user already exists
	exists, err := m.UserExists(username)
	if err != nil {
		return fmt.Errorf("failed to check if user exists: %w", err)
	}

	if exists {
		return ErrUserExists
	}

	// Execute saslpasswd2 command
	// -c: create user
	// -p: pipe password from stdin
	// -u: specify domain
	cmd := exec.Command("saslpasswd2", "-c", "-p", "-u", m.domain, username)

	// Pipe password to stdin
	cmd.Stdin = strings.NewReader(password)

	// Capture output for logging
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	m.logger.Debug().
		Str("username", username).
		Str("domain", m.domain).
		Msg("Creating SASL user")

	if err := cmd.Run(); err != nil {
		m.logger.Error().
			Err(err).
			Str("username", username).
			Str("stderr", stderr.String()).
			Msg("Failed to create SASL user")

		return fmt.Errorf("%w: %v (stderr: %s)", ErrCommandFailed, err, stderr.String())
	}

	m.logger.Info().
		Str("username", username).
		Str("domain", m.domain).
		Msg("SASL user created successfully")

	return nil
}

// DeleteUser deletes a SASL user
func (m *UserManager) DeleteUser(username string) error {
	// Validate username to prevent injection attacks
	if err := validateUsername(username); err != nil {
		return err
	}

	// Check if user exists
	exists, err := m.UserExists(username)
	if err != nil {
		return fmt.Errorf("failed to check if user exists: %w", err)
	}

	if !exists {
		return ErrUserNotFound
	}

	// Execute saslpasswd2 command
	// -d: delete user
	// -u: specify domain
	cmd := exec.Command("saslpasswd2", "-d", "-u", m.domain, username)

	// Capture output for logging
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	m.logger.Debug().
		Str("username", username).
		Str("domain", m.domain).
		Msg("Deleting SASL user")

	if err := cmd.Run(); err != nil {
		m.logger.Error().
			Err(err).
			Str("username", username).
			Str("stderr", stderr.String()).
			Msg("Failed to delete SASL user")

		return fmt.Errorf("%w: %v (stderr: %s)", ErrCommandFailed, err, stderr.String())
	}

	m.logger.Info().
		Str("username", username).
		Str("domain", m.domain).
		Msg("SASL user deleted successfully")

	return nil
}

// ListUsers lists all SASL users in the domain
func (m *UserManager) ListUsers() ([]User, error) {
	// Execute sasldblistusers2 command
	cmd := exec.Command("sasldblistusers2", "-f", m.sasldbPath)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	m.logger.Debug().
		Str("domain", m.domain).
		Msg("Listing SASL users")

	if err := cmd.Run(); err != nil {
		m.logger.Error().
			Err(err).
			Str("stderr", stderr.String()).
			Msg("Failed to list SASL users")

		return nil, fmt.Errorf("%w: %v (stderr: %s)", ErrCommandFailed, err, stderr.String())
	}

	// Parse output
	// Format: username@domain: userPassword
	users := make([]User, 0)
	scanner := bufio.NewScanner(&stdout)

	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines
		if strings.TrimSpace(line) == "" {
			continue
		}

		// Parse line: username@domain: userPassword
		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 1 {
			continue
		}

		userDomain := strings.TrimSpace(parts[0])

		// Split username@domain
		userParts := strings.SplitN(userDomain, "@", 2)
		if len(userParts) != 2 {
			continue
		}

		username := userParts[0]
		domain := userParts[1]

		// Only include users from our domain
		if domain != m.domain {
			continue
		}

		users = append(users, User{
			Username:  username,
			Domain:    domain,
			CreatedAt: time.Now().Format(time.RFC3339), // Note: sasldb2 doesn't store creation time
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error scanning output: %w", err)
	}

	m.logger.Debug().
		Int("count", len(users)).
		Str("domain", m.domain).
		Msg("Listed SASL users")

	return users, nil
}

// UserExists checks if a user exists in the SASL database
func (m *UserManager) UserExists(username string) (bool, error) {
	// Validate username to prevent injection attacks
	if err := validateUsername(username); err != nil {
		return false, err
	}

	users, err := m.ListUsers()
	if err != nil {
		return false, fmt.Errorf("failed to list users: %w", err)
	}

	for _, user := range users {
		if user.Username == username {
			return true, nil
		}
	}

	return false, nil
}

// UpdatePassword updates the password for an existing user
// This is implemented as a create operation with the -c flag which updates if exists
func (m *UserManager) UpdatePassword(username, password string) error {
	// Validate inputs to prevent injection attacks
	if err := validateUsername(username); err != nil {
		return err
	}

	if err := validatePassword(password); err != nil {
		return err
	}

	// Check if user exists
	exists, err := m.UserExists(username)
	if err != nil {
		return fmt.Errorf("failed to check if user exists: %w", err)
	}

	if !exists {
		return ErrUserNotFound
	}

	// Execute saslpasswd2 command (same as create, will update if exists)
	cmd := exec.Command("saslpasswd2", "-c", "-p", "-u", m.domain, username)

	// Pipe password to stdin
	cmd.Stdin = strings.NewReader(password)

	// Capture output for logging
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	m.logger.Debug().
		Str("username", username).
		Str("domain", m.domain).
		Msg("Updating SASL user password")

	if err := cmd.Run(); err != nil {
		m.logger.Error().
			Err(err).
			Str("username", username).
			Str("stderr", stderr.String()).
			Msg("Failed to update SASL user password")

		return fmt.Errorf("%w: %v (stderr: %s)", ErrCommandFailed, err, stderr.String())
	}

	m.logger.Info().
		Str("username", username).
		Str("domain", m.domain).
		Msg("SASL user password updated successfully")

	return nil
}
