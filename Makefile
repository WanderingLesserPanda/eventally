GOBIN := $(HOME)/go/bin
SQLC := $(GOBIN)/sqlc

.PHONY: all sqlc build run test tidy

all: sqlc build

sqlc:
	$(SQLC) generate

build:
	go build ./...

run:
	go run ./cmd/eventally

test:
	go test ./...

tidy:
	go mod tidy
