.PHONY: all build run test cluster-up cluster-down clean

# Build the main node binary
build:
	go build -o bin/chainpulse ./cmd/server

# Run the node locally (single node, no cluster)
run:
	go run ./cmd/server

# Run all tests
test:
	go test ./... -v -race

# Start a local cluster via docker-compose
cluster-up:
	docker compose -f docker-compose.yml up --build --force-recreate -d

# Tear down the cluster
cluster-down:
	docker compose -f docker-compose.yml down -v

# Stream logs from all node
cluster-logs:
	docker compose -f docker-compose.yml logs -f

# Kill whatever is running on port 8080
kill:
	-lsof -ti:8080 | xargs kill -9 2>/dev/null || true

# Clean built binaries
clean:
	rm -rf bin/
