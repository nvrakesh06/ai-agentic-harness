EXE := $(shell go env GOEXE)

.PHONY: build install test vet demo release
build:
	go build -trimpath -o bin/aih$(EXE) ./cmd/aih
install:
	go install ./cmd/aih
test:
	go test ./... -timeout 6m
vet:
	go vet ./...
demo: build
	./bin/aih$(EXE) demo
release:
	go run ./cmd/release
