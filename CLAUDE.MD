# Relay Agent

Postfix mail relay monitoring and management service written in Go. Parses Postfix logs in real-time, stores email delivery records in MongoDB, and exposes a REST API for queue management and SMTP user administration.

## Build & Test

```bash
make build          # Production: Linux AMD64, static, stripped -> bin/relay-agent
make build-local    # Native platform build
make test           # go test -v -race ./...
make test-coverage  # Tests + coverage.html
make bench          # Parser benchmarks (internal/parser)
make lint           # golangci-lint (must be installed)
make fmt            # go fmt ./...
make run            # go run ./cmd/relay-agent -config config/config.yaml
make deploy         # Build + install to /opt/relay-agent + restart systemd service
```

## Project Structure

```
cmd/relay-agent/main.go          # Entry point, wires all components
internal/
  api/                           # REST API (server, handlers, middleware)
  config/                        # YAML config loading with env var overrides (RELAY_<SECTION>_<FIELD>)
  filter/                        # SMTP content filter (:10025) + connection pool
  parser/                        # Postfix log parser (zero-alloc, 32-shard FNV-1a map)
  postfix/                       # Queue management via postqueue/postsuper CLI
  repository/                    # MongoDB repository (models, bulk upsert, indexes)
  smtp/                          # SASL user management via saslpasswd2
  stats/                         # Lock-free in-memory stats (atomic counters)
  tailer/                        # inotify log tailer with rotation handling
  util/                          # Turkey timezone (UTC+3) helpers
```

## Architecture

```
Sender -> Postfix :587 (STARTTLS/SASL) -> content_filter -> SMTP Filter :10025
  [records email with X-Mailgateway-Queue-ID to MongoDB as "received"]
  -> re-inject to Postfix :10026 -> delivers to internet
  -> log parser tails /var/log/mail.log -> updates MongoDB with final status
  -> MongoDB Change Stream notifies downstream consumers (< 100ms)
```

| Service              | Port  |
|----------------------|-------|
| relay-agent REST API | 8080  |
| SMTP Filter          | 10025 |
| Postfix Submission   | 587   |
| Postfix Reinjection  | 10026 |
| MongoDB              | 27017 |

## Code Conventions

- **Language:** Go 1.22, standard `net/http` (no web framework). Code and comments in English.
- **Logging:** zerolog (structured JSON). Use `log.Info()`, `log.Error()` etc.
- **Config:** YAML via `config.example.yaml`. Production config at `/opt/relay-agent/config/config.yaml` (gitignored). Env var overrides: `RELAY_<SECTION>_<FIELD>`.
- **API auth:** `X-API-Secret` header, minimum 16 characters.
- **Credentials:** Stored in `credentials/` directory (gitignored), never committed.
- **Testing:** Unit tests in `*_test.go`, benchmarks in `benchmark_test.go`, examples in `example_test.go`.
- **Struct fields:** Ordered by size (largest first) for optimal memory alignment.

## Performance Patterns (Do Not Regress)

These optimizations are intentional and must be preserved:

- **Zero-allocation log parsing** in `internal/parser/` — uses `strings.Index`/`strings.IndexByte` instead of regex on the hot path. Regex patterns in `patterns.go` exist for reference only.
- **32-shard concurrent map** (`sharded_map.go`) with FNV-1a hashing — reduces lock contention ~95%.
- **sync.Pool** for Email/LogEntry structs (`GetEmail()`/`PutEmail()`) — reduces GC pressure ~50x.
- **Batch MongoDB upserts** — batches of 100, flushed every 5s, reduces DB load >90%.
- **Lock-free statistics** — uses `atomic.Int64` and `sync.Map`, no mutex on hot path.
- **Dual tracking** — SMTP filter captures `X-Mailgateway-Queue-ID` at receive time; log parser upserts delivery outcomes keyed on that ID.

## Key Design Decisions

- All timestamps stored in Turkey time (UTC+3) — use `util.NowTurkey()` and `util.ToTurkey()`.
- Change Streams over webhooks — downstream consumers connect directly to MongoDB replica set.
- MongoDB requires replica set `rs0` (needed for Change Streams).
- Input validation on usernames: alphanumeric + underscore only, 64 char max.
- Shell injection prevention: passwords passed via stdin to saslpasswd2, never as args.

## GitHub & Releases

Repo: `https://github.com/dtosun-cmyk/relay-agent` (branch: `master`)

```bash
make release TAG=v1.x.x          # Tag, create GitHub release, upload binary
make release-upload TAG=v1.x.x   # Re-upload binary to existing release
```
