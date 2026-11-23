.PHONY: all build web server proto test docker-build docker-up docker-down clean

all: build

proto:
	protoc -I proto --go_out=internal/pb --go_opt=paths=source_relative --go_opt=Mrendezvous.proto=github.com/KriszW/godesk-plane/internal/pb proto/rendezvous.proto

web:
	cd web && npm install && npm run build

server:
	CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags="-s -w" -o bin/godesk-plane ./cmd/godesk-plane

build: web server

test:
	GOTOOLCHAIN=local go test -v ./...

docker-build:
	docker compose build

docker-up:
	docker compose up -d

docker-down:
	docker compose down

clean:
	rm -rf bin/
