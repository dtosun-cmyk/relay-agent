# Relay Agent

Postfix mail relay monitoring and management service written in Go. Parses Postfix logs in real-time, stores email delivery records in MongoDB, and exposes a REST API for management.

## Architecture

```
Sender -> Postfix :587 (STARTTLS/SASL) -> content_filter -> SMTP Filter :10025
  [records email with X-Mailgateway-Queue-ID to MongoDB as "received"]
  -> re-inject to Postfix :10026 -> delivers to internet
  -> log parser tails /var/log/mail.log -> updates MongoDB with final status
  -> MongoDB Change Stream notifies downstream consumers (< 100ms)
```

## Tech Stack

- **Go 1.22** — no web framework, standard `net/http`
- **MongoDB** with replica set `rs0` (required for Change Streams)
- **zerolog** for structured logging, **fsnotify** for inotify-based log tailing
- **Python 3** (motor + asyncpg) for mailgateway integration watcher only

## Project Structure

```
cmd/relay-agent/main.go          # Entry point, wires all components
internal/
  api/                           # REST API (server, handlers, middleware)
  config/                        # YAML config loading with env var overrides
  filter/                        # SMTP content filter (:10025) + connection pool
  parser/                        # Postfix log parser (zero-alloc, sharded map)
  postfix/                       # Queue management via postqueue/postsuper CLI
  repository/                    # MongoDB repository (models, bulk upsert, indexes)
  smtp/                          # SASL user management via saslpasswd2
  stats/                         # Lock-free in-memory stats (atomic counters)
  tailer/                        # inotify log tailer with rotation handling
  util/                          # Turkey timezone (UTC+3) helpers
mailgateway-integration/         # Python Change Stream watcher -> PostgreSQL
```

## Build & Test

```bash
make build          # Production: Linux AMD64, static, stripped -> bin/relay-agent
make build-local    # Native platform build
make test           # go test -v -race ./...
make test-coverage  # Tests + coverage.html
make bench          # Parser benchmarks
make lint           # golangci-lint (must be installed)
make fmt            # go fmt ./...
make deploy         # Build + install to /opt/relay-agent + restart systemd service
```

## Configuration

Template: `config.example.yaml`. Production: `/opt/relay-agent/config/config.yaml` (gitignored).

All config values can be overridden with env vars: `RELAY_<SECTION>_<FIELD>` (e.g., `RELAY_MONGODB_URI`).

API authentication: `X-API-Secret` header (min 16 chars).

## Key Design Decisions

- **Change Streams over webhooks** — downstream consumers connect directly to MongoDB replica set
- **Zero-alloc log parsing** — uses `strings.Index`/`strings.IndexByte` instead of regex on hot path; regex patterns in `patterns.go` are for reference only
- **32-shard concurrent map** (FNV-1a) in parser reduces lock contention ~95%
- **sync.Pool** for Email/LogEntry structs reduces GC pressure ~50x
- **Bulk MongoDB upserts** — batches of 100, flushed every 5s, reduces load >90%
- **Dual tracking** — SMTP filter captures `X-Mailgateway-Queue-ID` at receive time; log parser upserts delivery outcomes keyed on that ID
- **All timestamps** stored in Turkey time (UTC+3)

## Services & Ports

| Service              | Port  |
|----------------------|-------|
| relay-agent REST API | 8080  |
| SMTP Filter          | 10025 |
| Postfix Submission   | 587   |
| Postfix Reinjection  | 10026 |
| MongoDB              | 27017 |

## install.sh Known Issues & Fixes (2026-02-24)

### 1. Postfix logları /var/log/mail.log'a yazılmıyordu
**Sorun:** rsyslog yapılandırması yoktu. Modern Ubuntu'da Postfix varsayılan olarak journald'a yazar, `/var/log/mail.log`'a değil.
**Çözüm:** `/etc/rsyslog.d/20-postfix.conf` oluşturuldu, `mail.*` facility'si mail.log'a yönlendirildi. logrotate kuralı eklendi.

### 2. Log formatı parser ile uyumsuzdu
**Sorun:** rsyslog ISO 8601 formatı yazıyordu (`2026-02-24T21:26:46+00:00`), parser BSD syslog formatı bekliyordu (`Feb 24 21:26:46`).
**Çözüm:** rsyslog kuralına `;RSYSLOG_TraditionalFileFormat` template'i eklendi.

### 3. Port 10025 Postfix ile çakışıyordu
**Sorun:** `master.cf`'de port 10025 Postfix smtpd olarak tanımlıydı. relay-agent SMTP filter bu porta bind edemiyordu. Emailler filter'ı atlayıp doğrudan Postfix içinde dolanıyordu, DB'ye hiç kayıt düşmüyordu.
**Çözüm:** Port 10025 `master.cf`'den kaldırıldı (relay-agent'a ait). Sadece port 10026 (reinjection) Postfix'te kalır. install.sh'a mevcut 10025 girişini temizleyen `sed` komutu eklendi.

## Conventions

- Language: Go code and comments in English, user-facing docs may be Turkish
- Logging: zerolog (structured JSON)
- Config changes: update `config.example.yaml` template too
- Credentials: stored in `credentials/` directory (gitignored), never committed
- Changes should be documented in `CHANGELOG.md`
