TOOLS_BIN = ./bin
MIGRATION_DIR = ./migration
POSTGRES_PASS = mysecretpassword
POSTGRES_DB = vpn-server

.PHONY: migration
migration: ${TOOLS_BIN}/goose
	mkdir -p $(MIGRATION_DIR)
	${TOOLS_BIN}/goose -dir $(MIGRATION_DIR) create $(name) sql

${TOOLS_BIN}/goose: export GOBIN = $(shell pwd)/$(TOOLS_BIN)
${TOOLS_BIN}/goose:
	mkdir -p ./bin
	go install github.com/pressly/goose/v3/cmd/goose@latest

.PHONY: migrate
migrate: ${TOOLS_BIN}/goose
	${TOOLS_BIN}/goose -dir $(MIGRATION_DIR) postgres "host=localhost port=5432 user=postgres password=$(POSTGRES_PASS) dbname=$(POSTGRES_DB) sslmode=disable" up

.PHONY: database_up
database_up:
	docker run -e POSTGRES_PASSWORD=$(POSTGRES_PASS) -e POSTGRES_DB=$(POSTGRES_DB) -p 5432:5432 -d postgres
	sleep 3

.PHONY: start
start: database_up migrate

.PHONY: build
build:
	GOOS="linux" GOARCH="amd64" go build -o ./bin/service-linux github.com/tonikpro/outline-ss-server/cmd/outline-ss-server
