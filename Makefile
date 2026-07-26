.PHONY: build run test generate docker docker-up docker-down clean

build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/katchup ./cmd/katchup

run:
	go run ./cmd/katchup

test:
	go test ./...

# Note: no `templ generate` here — the shipped *_templ.go files are
# hand-maintained Go and regenerating would break the build.
generate:
	sqlc generate

docker:
	docker compose build

docker-up:
	docker compose up -d

docker-down:
	docker compose down

clean:
	rm -rf bin/
	go clean
