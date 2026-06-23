.PHONY: run test seed
run:
	go run ./cmd/server
test:
	go test ./...
seed:
	DATABASE_PATH=$${DATABASE_PATH:-./data/app.db} go run ./cmd/server
