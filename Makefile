BINARY := dreamtui
PREFIX ?= /usr/local

.PHONY: build install test fmt vet clean

build:
	go build -o $(BINARY) ./cmd/dreamtui

install: build
	install -m 0755 $(BINARY) $(PREFIX)/bin/$(BINARY)

test:
	go test ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
