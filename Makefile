.PHONY: build test vet clean

BINARY := bin/voidshell

build:
	go build -o $(BINARY) ./cmd/voidshell

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin/
