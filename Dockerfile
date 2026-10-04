FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Generated code (sqlc, templ) is checked in and built as-is. Do NOT run
# `templ generate` here: the .templ files are stale hand-written Go, not
# valid templ sources, and regenerating from them breaks the build.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /katchup ./cmd/katchup

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget

# Fixed uid/gid (the Alpine defaults, pinned) so a bind-mounted data dir can be
# chowned to a known owner: `chown -R 100:101 ./data`.
RUN addgroup -S -g 101 app && adduser -S -u 100 -G app app

WORKDIR /app

# Migrations are embedded in the binary (sql/migrations/migrations.go),
# so only the binary is needed at runtime.
COPY --from=builder /katchup /app/katchup

RUN mkdir -p /app/data && chown -R app:app /app

USER app

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/katchup"]
