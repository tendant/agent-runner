BINARY := agent-runner
CMD := ./cmd/server
PREFIX := $(shell go env GOPATH)

.DEFAULT_GOAL := help
.PHONY: build install clean test help

build:
	go build -o $(BINARY) $(CMD)

install: build
	install -d $(PREFIX)/bin
	install -m 755 $(BINARY) $(PREFIX)/bin/$(BINARY)

clean:
	rm -f $(BINARY)

test:
	go test -race ./...

help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  build    Build the $(BINARY) binary"
	@echo "  install  Install to $$(go env GOPATH)/bin"
	@echo "  clean    Remove built binary"
	@echo "  test     Run tests with race detector"
	@echo "  help     Show this help"
