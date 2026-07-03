# Agent Build Instructions

## Prerequisites
- Go 1.26+
- sqlc (`go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`)
- templ (`go install github.com/a-h/templ/cmd/templ@latest`)
- goose (`go install github.com/pressly/goose/v3/cmd/goose@latest`)
- YubiKey Manager (`ykman`) for PIV key injection (one-time setup)

## Project Setup
```bash
# Install Go dependencies
go mod init katchup
go mod tidy

# Generate sqlc code from SQL queries
sqlc generate

# Generate templ templates
templ generate

# Build the binary
CGO_ENABLED=0 go build -o bin/katchup ./cmd/katchup
```

## Running Tests
```bash
# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run tests for a specific package
go test -v ./internal/account/
go test -v ./internal/imap/
go test -v ./internal/crypto/
go test -v ./internal/api/

# Run tests with coverage
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

## Build Commands
```bash
# Development build
go build -o bin/katchup ./cmd/katchup

# Production build (stripped binary)
CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/katchup ./cmd/katchup
```

## Development Server
```bash
# Run with environment variables
DB_PATH=data/katchup.db KATCHUP_LISTEN=:8080 go run ./cmd/katchup

# Or use .env file
source .env && go run ./cmd/katchup
```

## YubiKey PIV Setup (One-Time)
```bash
# Check YubiKey is connected
ykman list

# Enter PIV management mode (requires PIN)
ykman piv access change-pin

# Generate RSA-2048 key in slot 9a (authentication slot)
ykman piv generate-key -a rsa2048 9a

# Verify the key is in the slot
ykman piv information
```

## Code Generation
```bash
# After modifying sql/queries/*.sql:
sqlc generate

# After modifying internal/view/**/*.templ:
templ generate

# After adding new sql/migrations/*.sql:
# Migrations run automatically on startup via goose embedded
```

## Docker
```bash
# Build Docker image
docker compose build

# Run with Docker
docker compose up -d

# View logs
docker compose logs -f

# Stop
docker compose down
```

## Makefile Targets
```bash
make build      # Build binary
make run        # Run locally
make test       # Run tests
make generate   # Run sqlc + templ generate
make docker     # Build + run Docker
make clean      # Remove build artifacts
```

## Key Learnings
- Always use CGO_ENABLED=0 — the SQLite driver is pure Go (modernc.org/sqlite)
- sqlc-generated code lives in internal/database/ — never hand-edit those files
- templ files must be generated before building (templ generate creates *_templ.go files)
- Goose migrations run on startup via filepath.Join("sql", "migrations") — Dockerfile copies migrations to /app/sql/migrations/
- Standard net/http ServeMux for routing (no third-party router)
- YubiKey PIV operations: plaintext never leaves the container, keys never exported
- .eml files are stored in data/ — this directory is gitignored
- All timestamps stored as UTC in the database
- KATCHUP_MASTER_KEY env var required for encryption — set it for production, optional for dev
- KATCHUP_ENV=production enables JSON logging via log/slog; default is "development" (text logs)
- GET /health returns {"status":"ok"} for Docker HEALTHCHECK
- Encrypted files use .eml.enc extension with version-prefixed binary format
- Account store requires master key: account.New(db, masterKey)
- Syncer requires key wrapper: imap.NewSyncer(store, dataDir, keyWrapper, encStore)
- Download endpoint pattern: GET /download/{accountID}/{date}/{filename}
- Crypto package uses KeyWrapper interface — MasterKeyWrapper for dev, pluggable for YubiKey
