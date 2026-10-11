# Relay Agent

Postfix mail relay log parser and management service written in Go.

Parses Postfix logs in real-time, stores email delivery records in MongoDB, and exposes a REST API for queue management and SMTP user administration.

## Features

- Real-time Postfix log parsing (zero-alloc, sharded concurrent map)
- MongoDB storage with Change Streams support
- SMTP content filter for email tracking
- Postfix queue management API
- SASL user management API
- Bulk upsert with connection pooling
- Turkey timezone (UTC+3) support

## Requirements

- Ubuntu 22.04+ (or Debian-based)
- MongoDB 6.0+ (Replica Set)
- Postfix

## Quick Install

The install script downloads the pre-built binary, sets up MongoDB, Postfix, TLS, and systemd service:

```bash
curl -fsSL https://raw.githubusercontent.com/dtosun-cmyk/relay-agent/master/install.sh | sudo bash
```

Or clone and run:

```bash
git clone https://github.com/dtosun-cmyk/relay-agent.git
cd relay-agent
sudo ./install.sh
```

## Automated / Non-Interactive Install

The installer runs fully unattended when driven by environment variables — this
is how the Mail Gateway "one-click provisioning" feature installs the agent over
SSH. No TTY is required.

| Env var | Purpose | Default |
|---------|---------|---------|
| `RELAY_DOMAIN` | FQDN for this relay (TLS + Postfix). If unset in non-interactive mode, the detected hostname is used. | detected hostname |
| `RELAY_SERVER_ID` | Mail Gateway `relay_servers.id` this agent is linked to (written to `mailgateway.relay_server_id`). | `1` |
| `RELAY_LETSENCRYPT_EMAIL` | Email for the Let's Encrypt account (optional; without it the certificate is requested without an email). If Let's Encrypt fails, a self-signed cert is used. | *(none)* |
| `RELAY_ALLOWED_IPS` | Comma-separated IPs/CIDRs allowed to reach the management API and the MongoDB `relay_agent` account (the Mail Gateway server). Loopback is always allowed. Mail Gateway's one-click provisioning always sets it. | *(no IP filter, secret/password only — a warning is printed)* |
| `RELAY_AGENT_BINARY` / `RELAY_AGENT_SHA256` | Install this local binary (checked against the SHA-256) instead of downloading the latest release. | *(download)* |
| `RELAY_AGENT_VERSION` | Version label reported in the result. | release tag |

```bash
RELAY_SERVER_ID=7 RELAY_DOMAIN=relay1.example.com \
RELAY_LETSENCRYPT_EMAIL=admin@example.com \
  bash install.sh
```

On success the installer prints a single machine-readable line that automation
can parse (the human-readable summary is still printed above it):

```
###RELAY_RESULT### {"server_ip":"...","api_url":"https://...:8080","api_port":8080,"api_secret":"...","api_tls_cert":"<base64 PEM>","mongo_host":"...","mongo_port":27017,"mongo_database":"relay_logs","mongo_username":"relay_agent","mongo_password":"...","mongo_replica_set":"rs0","mongo_auth_source":"relay_logs","mongo_tls":true,"tls":"letsencrypt|selfsigned","smtp_tls_fingerprint":"AA:BB:...","smtp_hostname":"relay1.example.com","smtp_username":"...","smtp_password":"...","smtp_realm":"...","agent_version":"v1.0.5","relay_server_id":7}
```

The `###RELAY_RESULT###` marker and JSON shape are kept stable across releases
(fields are only added). Secrets are printed only in this line and in
`/opt/relay-agent/credentials/install-credentials.txt` (0600); in non-interactive
runs the human-readable summary hides them.

## Security (v1.0.5)

- **Management API is HTTPS only** with the agent's own certificate
  (`/opt/relay-agent/tls/agent.crt`, self-signed, 10 years, SAN = server IP + domain).
  Mail Gateway pins exactly this certificate (`api_tls_cert` in the result), so
  Let's Encrypt renewals never break the pin.
- **IP allow-list:** `server.allowed_ips` in `config.yaml` (from `RELAY_ALLOWED_IPS`);
  other sources get `403` before the secret is checked (an empty list disables the filter). The secret is compared in
  constant time; CORS is disabled.
- **MongoDB** runs with `net.tls.mode: preferTLS` (the agent certificate is also the
  `CAFile`, required by MongoDB 7+); the `admin` user may log in only from the server
  itself, the `relay_agent` user from `RELAY_ALLOWED_IPS` + the server itself (no IP
  restriction when the variable is empty).
- **Firewall:** the installer does not change the firewall. Open 587 (SMTP from the
  Mail Gateway), 8080 (API) and 27017 (MongoDB) **only to the Mail Gateway server**.
- **Tracking:** the agent links each delivery to Mail Gateway's
  `X-Mailgateway-Queue-ID`; if that link is not in memory any more (agent restart,
  long-deferred mail) the final result is matched by the relay queue ID.
- A Postfix without a systemd unit (built from source / vendor package) gets a unit,
  `postfix set-permissions` and unchrooted services; `inet_interfaces` is set to `all`.

## Build from Source

```bash
make build          # Linux AMD64, static, stripped -> bin/relay-agent
make test           # go test -v -race ./...
make deploy         # Build + install + restart systemd
```

## Configuration

Copy example config and edit:
```bash
cp config.example.yaml config/config.yaml
nano config/config.yaml
```

All values can be overridden with env vars: `RELAY_<SECTION>_<FIELD>`.

## API Endpoints

### Queue Management
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/queue` | Queue statistics |
| GET | `/api/queue/messages?limit=50&offset=0` | List messages |
| DELETE | `/api/queue/messages/{queue_id}` | Delete message |
| DELETE | `/api/queue/messages?confirm=yes` | Delete all |
| POST | `/api/queue/messages/{queue_id}/requeue` | Requeue message |
| POST | `/api/queue/messages/{queue_id}/hold` | Hold message |
| POST | `/api/queue/messages/{queue_id}/release` | Release from hold |
| POST | `/api/queue/flush` | Flush queue |

### SMTP Users
| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/smtp-users` | Create SMTP user |
| GET | `/api/smtp-users` | List SMTP users |
| DELETE | `/api/smtp-users/{username}` | Delete SMTP user |

Authentication: `X-API-Secret` header required for all endpoints.

## License

MIT
