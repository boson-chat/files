.PHONY: deps run build test test-unit test-integration dev-up dev-down lint migrate-up migrate-down migrate-create docker

ifneq (,$(wildcard .env))
include .env
export
endif

# docker-compose.yml: Postgres on 5442, MinIO on 9000 (bucket files-test).
TEST_DATABASE_URL ?= postgres://files:files@127.0.0.1:5442/files?sslmode=disable
TEST_S3_ENDPOINT ?= http://127.0.0.1:9000
TEST_S3_ACCESS_KEY ?= minio
TEST_S3_SECRET_KEY ?= minio-secret

deps:
	go mod download

run:
	go run ./cmd serve

build:
	CGO_ENABLED=0 go build -trimpath -o bin/files ./cmd

test: test-unit

# Unit tests; Postgres/MinIO tests skip unless TEST_DATABASE_URL / TEST_S3_ENDPOINT are set.
test-unit:
	go test -race ./...

dev-up:
	docker compose up -d --wait postgres minio

dev-down:
	docker compose down

# Everything, against the docker-compose Postgres and MinIO (make dev-up).
test-integration:
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' TEST_S3_ENDPOINT='$(TEST_S3_ENDPOINT)' \
	TEST_S3_ACCESS_KEY='$(TEST_S3_ACCESS_KEY)' TEST_S3_SECRET_KEY='$(TEST_S3_SECRET_KEY)' \
		go test -race -count=1 ./...

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

migrate-create:
	migrate create -ext sql -dir db/migrations -seq $(name)

migrate-up:
	go run ./cmd migrate up

migrate-down:
	go run ./cmd migrate down

docker:
	docker build -t boson-files:dev .
