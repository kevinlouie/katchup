# Go Project Patterns

Follow these patterns from existing projects in the workspace (especially `mayo`).

## Module & Dependencies
- Module path: `katchup` (simple, no GitHub path needed)
- Go version: 1.26
- CGO_ENABLED=0 always
- Minimal dependencies — prefer standard library

## Directory Layout
```
cmd/katchup/main.go     # Single entry point
internal/               # All packages (no pkg/)
sql/migrations/         # Goose SQL migrations
sql/queries/            # sqlc SQL query files
static/                 # CSS, JS
data/                   # .eml files + SQLite DB (gitignored)
```

## Database
- SQLite with WAL mode: `db.Exec("PRAGMA journal_mode=WAL")`
- Pure Go driver: `modernc.org/sqlite`
- Import as: `_ "modernc.org/sqlite"`
- Driver name: `"sqlite"`
- sqlc for type-safe queries
- Goose for migrations, embedded in binary:
  ```go
  import "github.com/pressly/goose/v3"
  //go:embed sql/migrations/*.sql
  var migrations embed.FS
  goose.SetBaseFS(migrations)
  goose.Up(db, "sql/migrations")
  ```

## sqlc Configuration
```yaml
version: "2"
sql:
  - engine: sqlite
    queries: sql/queries/
    schema: sql/migrations/
    gen:
      go:
        package: database
        out: internal/database
        emit_json_tags: true
        emit_empty_slices: true
```

## HTTP Server
- Standard `net/http.ServeMux` (Go 1.22+ pattern matching)
- Pattern examples: `GET /accounts/{id}`, `POST /accounts/new`
- Handler functions, not handler structs
- Dependency injection via closure:
  ```go
  func handleAccountList(db *database.Queries) http.HandlerFunc {
      return func(w http.ResponseWriter, r *http.Request) {
          // ...
      }
  }
  ```

## Templates (templ)
- Organized by feature: `internal/view/{feature}/{template}.templ`
- Base layout component wraps all pages
- Components accept typed Go structs, not maps
- Generate with: `templ generate`

## Configuration
- Environment variables with sensible defaults
- No config files — keep it simple
- Parse in `internal/config/config.go`:
  ```go
  func Load() Config {
      return Config{
          DBPath:       getEnv("DB_PATH", "data/katchup.db"),
          Listen:       getEnv("KATCHUP_LISTEN", ":8080"),
          // ...
      }
  }
  ```

## Docker
- Multi-stage: `golang:1.26-alpine` builder → `alpine:3.21` runtime
- Install sqlc + templ in builder stage
- Copy migrations + static + templates into image
- Non-root user
- HEALTHCHECK with wget
- Volumes for data/

## Logging
- `log/slog` (standard library structured logging)
- JSON handler for production, text handler for development

## Error Handling
- Return errors up the call stack
- Log at the handler level
- HTTP 500 for unexpected errors with generic user message
- Validation errors rendered inline in forms

## Testing
- Table-driven tests
- In-memory SQLite for database tests: `file::memory:?cache=shared`
- httptest for HTTP handler tests
- Interface-based mocking (no mocking framework)
