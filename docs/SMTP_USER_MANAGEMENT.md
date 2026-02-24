# SMTP User Management Integration Guide

This document provides a comprehensive guide for integrating and using the SMTP user management functionality in the relay-agent project.

## Overview

The SMTP user management module allows you to create, delete, and manage SASL users for SMTP authentication through a secure REST API. This feature is essential for managing authenticated SMTP relay users dynamically.

## Architecture

```
┌─────────────────┐
│   HTTP Client   │
│  (curl/Postman) │
└────────┬────────┘
         │ X-API-Secret
         ▼
┌─────────────────────────────┐
│   API Server (handlers.go)  │
│  /api/smtp-users            │
└────────┬────────────────────┘
         │
         ▼
┌─────────────────────────────┐
│  UserManager (users.go)     │
│  - Input Validation         │
│  - Security Checks          │
└────────┬────────────────────┘
         │
         ▼
┌─────────────────────────────┐
│    saslpasswd2 Command      │
│    /etc/sasldb2             │
└─────────────────────────────┘
```

## Files Created

### Core Implementation
- **`/opt/relay-agent/internal/smtp/users.go`** (8.7 KB)
  - Main user management implementation
  - UserManager struct and methods
  - Input validation and security features

- **`/opt/relay-agent/internal/smtp/users_test.go`** (7.5 KB)
  - Comprehensive unit tests
  - Integration tests (when enabled)
  - Benchmarks for validation functions

- **`/opt/relay-agent/internal/smtp/example_test.go`** (4.4 KB)
  - Example usage patterns
  - API integration examples

- **`/opt/relay-agent/internal/smtp/README.md`** (7.2 KB)
  - Module documentation
  - API reference
  - Security guidelines

### API Integration
- **`/opt/relay-agent/internal/api/handlers.go`** (updated)
  - Added SMTP user endpoints
  - API secret authentication
  - Error handling

- **`/opt/relay-agent/internal/api/server.go`** (updated)
  - Added UserManager integration
  - Route configuration

### Configuration
- **`/opt/relay-agent/internal/config/config.go`** (updated)
  - Added SMTP section
  - Environment variable support
  - Validation rules

- **`/opt/relay-agent/config.example.yaml`** (updated)
  - SMTP configuration example
  - Environment variable examples

### Documentation & Examples
- **`/opt/relay-agent/examples/smtp_api_examples.sh`**
  - Bash script demonstrating all API operations
  - Includes success and error cases

## Installation

### 1. Install SASL Tools

On Ubuntu/Debian:
```bash
sudo apt-get update
sudo apt-get install sasl2-bin
```

On CentOS/RHEL:
```bash
sudo yum install cyrus-sasl-plain
```

### 2. Configure Permissions

```bash
# Set proper ownership for sasldb2
sudo chown root:sasl /etc/sasldb2
sudo chmod 640 /etc/sasldb2

# Add your user to the sasl group
sudo usermod -a -G sasl $(whoami)

# Re-login or use newgrp to activate group membership
newgrp sasl
```

### 3. Update Configuration

Edit `/opt/relay-agent/config/config.yaml`:

```yaml
smtp:
  domain: "smtp1.mxgate.com.tr"
  api_secret: "your-secure-api-secret-min-16-chars"
```

Or use environment variables:

```bash
export RELAY_SMTP_DOMAIN="smtp1.mxgate.com.tr"
export RELAY_SMTP_API_SECRET="production-secret-key-here"
```

### 4. Build and Run

```bash
cd /opt/relay-agent
go build -o bin/relay-agent ./cmd/relay-agent
./bin/relay-agent -config ./config/config.yaml
```

## API Usage

### Authentication

All SMTP user management endpoints require the `X-API-Secret` header:

```bash
X-API-Secret: your-secure-api-secret-min-16-chars
```

### Endpoints

#### 1. List All Users

```bash
curl -X GET http://localhost:8080/api/smtp-users \
  -H "X-API-Secret: your-secure-api-secret-min-16-chars"
```

**Response (200 OK):**
```json
{
  "users": [
    {
      "username": "user1",
      "domain": "smtp1.mxgate.com.tr",
      "created_at": "2025-12-22T15:00:00Z"
    }
  ],
  "count": 1
}
```

#### 2. Create User

```bash
curl -X POST http://localhost:8080/api/smtp-users \
  -H "X-API-Secret: your-secure-api-secret-min-16-chars" \
  -H "Content-Type: application/json" \
  -d '{
    "username": "newuser",
    "password": "SecurePass123"
  }'
```

**Response (201 Created):**
```json
{
  "message": "User created successfully",
  "username": "newuser"
}
```

**Error Responses:**
- `400 Bad Request` - Invalid username/password format
- `401 Unauthorized` - Invalid or missing API secret
- `409 Conflict` - User already exists
- `500 Internal Server Error` - Command execution failed

#### 3. Delete User

```bash
curl -X DELETE http://localhost:8080/api/smtp-users/username \
  -H "X-API-Secret: your-secure-api-secret-min-16-chars"
```

**Response (200 OK):**
```json
{
  "message": "User deleted successfully",
  "username": "username"
}
```

**Error Responses:**
- `400 Bad Request` - Invalid username format
- `401 Unauthorized` - Invalid or missing API secret
- `404 Not Found` - User does not exist
- `500 Internal Server Error` - Command execution failed

## Security Features

### 1. Input Validation

**Username Rules:**
- Only alphanumeric characters and underscores
- Maximum 64 characters
- Regex: `^[a-zA-Z0-9_]+$`

**Password Rules:**
- Minimum 8 characters
- Maximum 256 characters
- No character restrictions (supports complex passwords)

### 2. Shell Injection Prevention

- Strict username validation prevents shell metacharacters
- Passwords passed via stdin (not command line arguments)
- No user input directly passed to shell

### 3. API Authentication

- Shared secret authentication via `X-API-Secret` header
- Secret must be minimum 16 characters
- Failed authentication attempts are logged

### 4. Audit Logging

All operations are logged with:
- Timestamp
- Operation type (create/delete)
- Username
- Client IP address
- Success/failure status

## Testing

### Unit Tests

Run unit tests (no system dependencies):

```bash
cd /opt/relay-agent
go test ./internal/smtp -v
```

### Integration Tests

Run integration tests (requires saslpasswd2):

```bash
cd /opt/relay-agent
INTEGRATION_TESTS=1 go test ./internal/smtp -v
```

### Benchmarks

Run performance benchmarks:

```bash
cd /opt/relay-agent
go test ./internal/smtp -bench=. -benchmem
```

Expected results:
```
BenchmarkValidateUsername-4   	 3182642	       393.8 ns/op	       0 B/op	       0 allocs/op
BenchmarkValidatePassword-4   	1000000000	     0.3872 ns/op	       0 B/op	       0 allocs/op
```

### API Testing Script

Run the comprehensive API test script:

```bash
cd /opt/relay-agent
./examples/smtp_api_examples.sh
```

## Troubleshooting

### Permission Denied Errors

**Problem:** Cannot read/write `/etc/sasldb2`

**Solution:**
```bash
sudo chown root:sasl /etc/sasldb2
sudo chmod 640 /etc/sasldb2
sudo usermod -a -G sasl $(whoami)
newgrp sasl
```

### Command Not Found

**Problem:** `saslpasswd2: command not found`

**Solution:**
```bash
# Ubuntu/Debian
sudo apt-get install sasl2-bin

# CentOS/RHEL
sudo yum install cyrus-sasl-plain
```

### 401 Unauthorized

**Problem:** API returns "Invalid API secret"

**Solution:**
- Verify `X-API-Secret` header is set correctly
- Check config.yaml has correct `smtp.api_secret`
- Ensure secret is at least 16 characters

### User Creation Fails

**Problem:** User creation returns 500 error

**Solution:**
```bash
# Check if sasldb2 exists and is writable
ls -la /etc/sasldb2

# Check saslpasswd2 is executable
which saslpasswd2

# Test manually
sudo saslpasswd2 -c -u smtp.example.com testuser
```

## Production Deployment

### 1. Security Hardening

- Use HTTPS only (configure reverse proxy)
- Generate strong API secret (32+ characters)
- Implement rate limiting (e.g., nginx)
- Monitor failed authentication attempts

### 2. Monitoring

Add monitoring for:
- Failed authentication attempts
- User creation/deletion events
- API response times
- SASL database size

### 3. Backup

```bash
# Backup sasldb2 regularly
sudo cp /etc/sasldb2 /backup/sasldb2.$(date +%Y%m%d)
```

### 4. Log Rotation

The relay-agent logs are automatically rotated. Ensure SMTP operations are included:

```bash
grep "SMTP user" /var/log/relay-agent/relay-agent.log
```

## Example Integration

### Python Client

```python
import requests

API_BASE = "http://localhost:8080"
API_SECRET = "your-secure-api-secret-min-16-chars"

headers = {
    "X-API-Secret": API_SECRET,
    "Content-Type": "application/json"
}

# Create user
response = requests.post(
    f"{API_BASE}/api/smtp-users",
    headers=headers,
    json={"username": "newuser", "password": "SecurePass123"}
)
print(response.json())

# List users
response = requests.get(
    f"{API_BASE}/api/smtp-users",
    headers=headers
)
print(response.json())

# Delete user
response = requests.delete(
    f"{API_BASE}/api/smtp-users/newuser",
    headers=headers
)
print(response.json())
```

### Go Client

```go
package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "net/http"
)

const (
    apiBase   = "http://localhost:8080"
    apiSecret = "your-secure-api-secret-min-16-chars"
)

type CreateUserRequest struct {
    Username string `json:"username"`
    Password string `json:"password"`
}

func main() {
    client := &http.Client{}

    // Create user
    reqBody, _ := json.Marshal(CreateUserRequest{
        Username: "newuser",
        Password: "SecurePass123",
    })

    req, _ := http.NewRequest("POST", apiBase+"/api/smtp-users", bytes.NewBuffer(reqBody))
    req.Header.Set("X-API-Secret", apiSecret)
    req.Header.Set("Content-Type", "application/json")

    resp, _ := client.Do(req)
    defer resp.Body.Close()

    fmt.Printf("Status: %d\n", resp.StatusCode)
}
```

## Performance

The module is optimized for high performance:

- **Zero allocations** for input validation
- **Compiled regex** patterns (once at package init)
- **Efficient command execution** with proper buffering
- **Concurrent-safe** operations

Benchmarks show:
- Username validation: ~394 ns/op with 0 allocations
- Password validation: ~0.39 ns/op with 0 allocations

## Future Enhancements

Potential future improvements:

1. **Bulk operations** - Create/delete multiple users
2. **Password update endpoint** - Change existing user password
3. **User quotas** - Limit number of users per domain
4. **Audit trail export** - Export user management history
5. **Integration with external auth** - LDAP/Active Directory sync

## Support

For issues or questions:

1. Check the logs: `/var/log/relay-agent/relay-agent.log`
2. Review the README: `/opt/relay-agent/internal/smtp/README.md`
3. Run the test script: `/opt/relay-agent/examples/smtp_api_examples.sh`

## License

Part of the relay-agent project.
