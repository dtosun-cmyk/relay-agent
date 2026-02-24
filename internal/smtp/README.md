# SMTP User Management Module

This module provides secure SMTP SASL user management functionality for the relay-agent project. It manages SMTP authentication users via the `saslpasswd2` command-line tool.

## Features

- **Create SMTP Users**: Add new SASL users with secure password handling
- **Delete SMTP Users**: Remove existing SASL users
- **List SMTP Users**: View all users in the SASL domain
- **Update Passwords**: Change passwords for existing users
- **Security**: Input validation prevents shell injection attacks
- **Logging**: Comprehensive logging using zerolog

## Prerequisites

The system must have the following installed:

- `saslpasswd2` - For creating/deleting users
- `sasldblistusers2` - For listing users
- Proper permissions to access `/etc/sasldb2`

Install on Ubuntu/Debian:
```bash
sudo apt-get install sasl2-bin
```

Install on CentOS/RHEL:
```bash
sudo yum install cyrus-sasl-plain
```

## Usage

### Creating a UserManager

```go
import (
    "github.com/rs/zerolog"
    "relay-agent/internal/smtp"
)

logger := zerolog.New(os.Stdout).With().Timestamp().Logger()
domain := "smtp1.mxgate.com.tr"

manager := smtp.NewUserManager(domain, logger)
```

### Creating a User

```go
username := "john_doe"
password := "SecurePassword123"

err := manager.CreateUser(username, password)
if err != nil {
    switch err {
    case smtp.ErrInvalidUsername:
        log.Println("Invalid username format")
    case smtp.ErrInvalidPassword:
        log.Println("Password must be at least 8 characters")
    case smtp.ErrUserExists:
        log.Println("User already exists")
    default:
        log.Printf("Failed to create user: %v", err)
    }
}
```

### Listing Users

```go
users, err := manager.ListUsers()
if err != nil {
    log.Fatalf("Failed to list users: %v", err)
}

for _, user := range users {
    fmt.Printf("User: %s@%s\n", user.Username, user.Domain)
}
```

### Checking if User Exists

```go
exists, err := manager.UserExists("john_doe")
if err != nil {
    log.Fatalf("Failed to check user: %v", err)
}

if exists {
    fmt.Println("User exists!")
}
```

### Updating Password

```go
err := manager.UpdatePassword("john_doe", "NewSecurePassword456")
if err != nil {
    log.Fatalf("Failed to update password: %v", err)
}
```

### Deleting a User

```go
err := manager.DeleteUser("john_doe")
if err != nil {
    switch err {
    case smtp.ErrInvalidUsername:
        log.Println("Invalid username format")
    case smtp.ErrUserNotFound:
        log.Println("User does not exist")
    default:
        log.Printf("Failed to delete user: %v", err)
    }
}
```

## API Integration

The module is integrated with the relay-agent HTTP API. The following endpoints are available:

### List SMTP Users

```bash
GET /api/smtp-users
Header: X-API-Secret: your-api-secret
```

Response:
```json
{
  "users": [
    {
      "username": "john_doe",
      "domain": "smtp1.mxgate.com.tr",
      "created_at": "2023-01-01T12:00:00Z"
    }
  ],
  "count": 1
}
```

### Create SMTP User

```bash
POST /api/smtp-users
Header: X-API-Secret: your-api-secret
Content-Type: application/json

{
  "username": "john_doe",
  "password": "SecurePassword123"
}
```

Response (201 Created):
```json
{
  "message": "User created successfully",
  "username": "john_doe"
}
```

Error Responses:
- `400 Bad Request` - Invalid username or password
- `401 Unauthorized` - Invalid API secret
- `409 Conflict` - User already exists
- `500 Internal Server Error` - Command execution failed

### Delete SMTP User

```bash
DELETE /api/smtp-users/{username}
Header: X-API-Secret: your-api-secret
```

Response (200 OK):
```json
{
  "message": "User deleted successfully",
  "username": "john_doe"
}
```

Error Responses:
- `400 Bad Request` - Invalid username
- `401 Unauthorized` - Invalid API secret
- `404 Not Found` - User does not exist
- `500 Internal Server Error` - Command execution failed

## Security Features

### Input Validation

1. **Username Validation**:
   - Only alphanumeric characters and underscores allowed
   - Maximum length: 64 characters
   - Prevents shell injection attacks

2. **Password Validation**:
   - Minimum length: 8 characters
   - Maximum length: 256 characters

3. **Shell Injection Prevention**:
   - Strict regex validation on usernames
   - No shell metacharacters allowed
   - Password passed via stdin (not command line)

### API Authentication

All SMTP user management endpoints require the `X-API-Secret` header:

```bash
curl -X GET http://localhost:8080/api/smtp-users \
  -H "X-API-Secret: your-secure-api-secret-min-16-chars"
```

The API secret is configured in the `config.yaml` file:

```yaml
smtp:
  domain: "smtp1.mxgate.com.tr"
  api_secret: "your-secure-api-secret-min-16-chars"
```

**Important**: The API secret must be at least 16 characters for security.

## Configuration

Add the following to your `config.yaml`:

```yaml
smtp:
  domain: "smtp1.mxgate.com.tr"           # SASL domain
  api_secret: "your-secure-api-secret"    # Min 16 characters
```

Environment variable overrides:

```bash
export RELAY_SMTP_DOMAIN="smtp2.mxgate.com.tr"
export RELAY_SMTP_API_SECRET="production-secret-key"
```

## Testing

### Unit Tests

Run unit tests (no SASL commands executed):

```bash
go test ./internal/smtp -v
```

### Integration Tests

Integration tests require `saslpasswd2` installed and proper permissions:

```bash
INTEGRATION_TESTS=1 go test ./internal/smtp -v
```

### Benchmarks

Run performance benchmarks:

```bash
go test ./internal/smtp -bench=. -benchmem
```

## Error Handling

The module defines the following errors:

- `ErrInvalidUsername` - Username validation failed
- `ErrInvalidPassword` - Password validation failed
- `ErrUserExists` - User already exists (on create)
- `ErrUserNotFound` - User does not exist (on delete/update)
- `ErrCommandFailed` - SASL command execution failed

## Examples

See `example_test.go` for comprehensive usage examples:

```bash
go test ./internal/smtp -run=Example
```

## Performance

The module is optimized for performance:

- Zero allocations for input validation
- Regex compiled once at package initialization
- Efficient command execution with proper buffering

Benchmark results:
```
BenchmarkValidateUsername-4   	 3182642	       393.8 ns/op	       0 B/op	       0 allocs/op
BenchmarkValidatePassword-4   	1000000000	     0.3872 ns/op	       0 B/op	       0 allocs/op
```

## Security Best Practices

1. **Secure API Secret**: Use a strong, random API secret (min 16 chars)
2. **HTTPS Only**: Always use HTTPS in production for API calls
3. **Rate Limiting**: Implement rate limiting on SMTP user endpoints
4. **Audit Logging**: All operations are logged with zerolog
5. **Principle of Least Privilege**: Run with minimal required permissions

## Troubleshooting

### Permission Denied

If you get permission errors:

```bash
sudo chown root:sasl /etc/sasldb2
sudo chmod 640 /etc/sasldb2
sudo usermod -a -G sasl <your-user>
```

### Command Not Found

Install SASL tools:

```bash
# Ubuntu/Debian
sudo apt-get install sasl2-bin

# CentOS/RHEL
sudo yum install cyrus-sasl-plain
```

### Verify Installation

```bash
# Check if commands are available
which saslpasswd2
which sasldblistusers2

# List current users
sudo sasldblistusers2
```

## License

Part of the relay-agent project.
