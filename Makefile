BINARY := agent-runner
CLI_BINARY := agent-cli
CMD := ./cmd/server
CLI_CMD := ./cmd/cli
PREFIX := $(shell go env GOPATH)

.DEFAULT_GOAL := help
.PHONY: build build-cli install clean test help

build:
	go build -o $(BINARY) $(CMD)

build-cli:
	go build -o $(CLI_BINARY) $(CLI_CMD)

install: build build-cli
	install -d $(PREFIX)/bin
	install -m 755 $(BINARY) $(PREFIX)/bin/$(BINARY)
	install -m 755 $(CLI_BINARY) $(PREFIX)/bin/$(CLI_BINARY)

clean:
	rm -f $(BINARY) $(CLI_BINARY)

test:
	go test -race ./...

help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  build      Build the $(BINARY) binary"
	@echo "  build-cli  Build the $(CLI_BINARY) binary"
	@echo "  install    Install both binaries to $$(go env GOPATH)/bin"
	@echo "  clean      Remove built binaries"
	@echo "  test       Run tests with race detector"
	@echo "  help       Show this help"
