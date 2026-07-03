.PHONY: build run test generate docker docker-up docker-down clean

build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/katchup ./cmd/katchup

run:
	go run ./cmd/katchup

test:
	go test ./...

generate:
	sqlc generate
	templ generate

docker:
	docker compose build

docker-up:
	docker compose up -d

docker-down:
	docker compose down

clean:
	rm -rf bin/
	go clean
