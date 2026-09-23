EXE := $(shell go env GOEXE)

.PHONY: build install test vet lint demo release
build:
	go build -trimpath -o bin/aih$(EXE) ./cmd/aih
install:
	go install ./cmd/aih
test:
	go test -p 1 ./... -timeout 10m
vet:
	go vet ./...
lint:
	go run ./cmd/checkfmt
	go vet ./...
demo: build
	./bin/aih$(EXE) demo
release:
	go run ./cmd/release
