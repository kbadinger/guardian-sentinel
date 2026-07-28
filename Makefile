BINARY := guardian-sentinel

.PHONY: build test lint fmt clean

build:
	go build -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test -race ./...

lint:
	gofmt -l . && go vet ./... && staticcheck ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin/
