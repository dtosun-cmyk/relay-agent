package smtp

import (
	"bytes"
	"os"
	"testing"

	"github.com/rs/zerolog"
)

// setupTestLogger creates a logger for tests
func setupTestLogger() zerolog.Logger {
	var buf bytes.Buffer
	return zerolog.New(&buf).With().Timestamp().Logger()
}

func TestValidateUsername(t *testing.T) {
	tests := []struct {
		name     string
		username string
		wantErr  bool
	}{
		{
			name:     "valid alphanumeric username",
			username: "user123",
			wantErr:  false,
		},
		{
			name:     "valid with underscores",
			username: "test_user_123",
			wantErr:  false,
		},
		{
			name:     "empty username",
			username: "",
			wantErr:  true,
		},
		{
			name:     "username with special chars",
			username: "user@domain",
			wantErr:  true,
		},
		{
			name:     "username with shell metacharacters",
			username: "user; rm -rf /",
			wantErr:  true,
		},
		{
			name:     "username with pipe",
			username: "user|cat",
			wantErr:  true,
		},
		{
			name:     "username with dollar sign",
			username: "user$var",
			wantErr:  true,
		},
		{
			name:     "username with backticks",
			username: "user`ls`",
			wantErr:  true,
		},
		{
			name:     "username with spaces",
			username: "user name",
			wantErr:  true,
		},
		{
			name:     "username too long",
			username: "user1234567890123456789012345678901234567890123456789012345678901234567890",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUsername(tt.username)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateUsername() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{
			name:     "valid password",
			password: "password123",
			wantErr:  false,
		},
		{
			name:     "minimum length password",
			password: "12345678",
			wantErr:  false,
		},
		{
			name:     "too short password",
			password: "1234567",
			wantErr:  true,
		},
		{
			name:     "empty password",
			password: "",
			wantErr:  true,
		},
		{
			name:     "complex password with special chars",
			password: "P@ssw0rd!#$%",
			wantErr:  false,
		},
		{
			name:     "very long password",
			password: string(make([]byte, 300)),
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePassword(tt.password)
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePassword() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewUserManager(t *testing.T) {
	logger := setupTestLogger()
	domain := "test.example.com"

	manager := NewUserManager(domain, logger)

	if manager == nil {
		t.Fatal("NewUserManager() returned nil")
	}

	if manager.domain != domain {
		t.Errorf("NewUserManager() domain = %v, want %v", manager.domain, domain)
	}

	if manager.sasldbPath != "/etc/sasldb2" {
		t.Errorf("NewUserManager() sasldbPath = %v, want %v", manager.sasldbPath, "/etc/sasldb2")
	}
}

// TestUserManagerValidation tests input validation without actually executing commands
func TestUserManagerValidation(t *testing.T) {
	logger := setupTestLogger()
	manager := NewUserManager("test.example.com", logger)

	t.Run("CreateUser with invalid username", func(t *testing.T) {
		err := manager.CreateUser("user@invalid", "password123")
		if err == nil {
			t.Error("Expected error for invalid username, got nil")
		}
	})

	t.Run("CreateUser with invalid password", func(t *testing.T) {
		err := manager.CreateUser("validuser", "short")
		if err == nil {
			t.Error("Expected error for invalid password, got nil")
		}
	})

	t.Run("DeleteUser with invalid username", func(t *testing.T) {
		err := manager.DeleteUser("user@invalid")
		if err == nil {
			t.Error("Expected error for invalid username, got nil")
		}
	})

	t.Run("UpdatePassword with invalid username", func(t *testing.T) {
		err := manager.UpdatePassword("user@invalid", "password123")
		if err == nil {
			t.Error("Expected error for invalid username, got nil")
		}
	})

	t.Run("UpdatePassword with invalid password", func(t *testing.T) {
		err := manager.UpdatePassword("validuser", "short")
		if err == nil {
			t.Error("Expected error for invalid password, got nil")
		}
	})

	t.Run("UserExists with invalid username", func(t *testing.T) {
		_, err := manager.UserExists("user@invalid")
		if err == nil {
			t.Error("Expected error for invalid username, got nil")
		}
	})
}

// TestUserManagerIntegration tests actual SASL commands
// These tests require saslpasswd2 to be installed and proper permissions
// Skip if INTEGRATION_TESTS environment variable is not set
func TestUserManagerIntegration(t *testing.T) {
	if os.Getenv("INTEGRATION_TESTS") != "1" {
		t.Skip("Skipping integration tests. Set INTEGRATION_TESTS=1 to run.")
	}

	logger := setupTestLogger()
	domain := "test.example.com"
	manager := NewUserManager(domain, logger)

	testUsername := "testuser_integration"
	testPassword := "testpassword123"

	// Cleanup before and after test
	defer func() {
		_ = manager.DeleteUser(testUsername)
	}()
	_ = manager.DeleteUser(testUsername) // Clean up any existing user

	t.Run("CreateUser", func(t *testing.T) {
		err := manager.CreateUser(testUsername, testPassword)
		if err != nil {
			t.Fatalf("CreateUser() error = %v", err)
		}

		// Verify user exists
		exists, err := manager.UserExists(testUsername)
		if err != nil {
			t.Fatalf("UserExists() error = %v", err)
		}
		if !exists {
			t.Error("User should exist after creation")
		}
	})

	t.Run("CreateUser duplicate", func(t *testing.T) {
		err := manager.CreateUser(testUsername, testPassword)
		if err != ErrUserExists {
			t.Errorf("CreateUser() duplicate should return ErrUserExists, got %v", err)
		}
	})

	t.Run("ListUsers", func(t *testing.T) {
		users, err := manager.ListUsers()
		if err != nil {
			t.Fatalf("ListUsers() error = %v", err)
		}

		found := false
		for _, user := range users {
			if user.Username == testUsername && user.Domain == domain {
				found = true
				break
			}
		}

		if !found {
			t.Error("Created user should appear in ListUsers()")
		}
	})

	t.Run("UpdatePassword", func(t *testing.T) {
		newPassword := "newpassword123"
		err := manager.UpdatePassword(testUsername, newPassword)
		if err != nil {
			t.Fatalf("UpdatePassword() error = %v", err)
		}
	})

	t.Run("DeleteUser", func(t *testing.T) {
		err := manager.DeleteUser(testUsername)
		if err != nil {
			t.Fatalf("DeleteUser() error = %v", err)
		}

		// Verify user no longer exists
		exists, err := manager.UserExists(testUsername)
		if err != nil {
			t.Fatalf("UserExists() error = %v", err)
		}
		if exists {
			t.Error("User should not exist after deletion")
		}
	})

	t.Run("DeleteUser non-existent", func(t *testing.T) {
		err := manager.DeleteUser(testUsername)
		if err != ErrUserNotFound {
			t.Errorf("DeleteUser() non-existent should return ErrUserNotFound, got %v", err)
		}
	})

	t.Run("UpdatePassword non-existent", func(t *testing.T) {
		err := manager.UpdatePassword(testUsername, "password123")
		if err != ErrUserNotFound {
			t.Errorf("UpdatePassword() non-existent should return ErrUserNotFound, got %v", err)
		}
	})
}

// BenchmarkValidateUsername benchmarks username validation
func BenchmarkValidateUsername(b *testing.B) {
	username := "test_user_123"
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = validateUsername(username)
	}
}

// BenchmarkValidatePassword benchmarks password validation
func BenchmarkValidatePassword(b *testing.B) {
	password := "password123"
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = validatePassword(password)
	}
}
