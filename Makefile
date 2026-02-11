BINARY := agent-runner
CMD := ./cmd/server
PREFIX := /usr/local

.PHONY: build install clean test

build:
	go build -o $(BINARY) $(CMD)

install: build
	install -d $(PREFIX)/bin
	install -m 755 $(BINARY) $(PREFIX)/bin/$(BINARY)

clean:
	rm -f $(BINARY)

test:
	go test -race ./...
