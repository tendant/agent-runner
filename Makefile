BINARY := agent-runner
CLI_BINARY := agent-cli
WECHAT_LOGIN_BINARY := wechat-login
CMD := ./cmd/server
CLI_CMD := ./cmd/cli
WECHAT_LOGIN_CMD := ./cmd/wechat-login
PREFIX := $(shell go env GOPATH)

.DEFAULT_GOAL := help
.PHONY: build build-cli build-wechat-login install clean test help

build:
	go build -o $(BINARY) $(CMD)

build-cli:
	go build -o $(CLI_BINARY) $(CLI_CMD)

build-wechat-login:
	go build -o $(WECHAT_LOGIN_BINARY) $(WECHAT_LOGIN_CMD)

install: build build-cli build-wechat-login
	install -d $(PREFIX)/bin
	install -m 755 $(BINARY) $(PREFIX)/bin/$(BINARY)
	install -m 755 $(CLI_BINARY) $(PREFIX)/bin/$(CLI_BINARY)
	install -m 755 $(WECHAT_LOGIN_BINARY) $(PREFIX)/bin/$(WECHAT_LOGIN_BINARY)

clean:
	rm -f $(BINARY) $(CLI_BINARY) $(WECHAT_LOGIN_BINARY)

test:
	go test -race ./...

help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  build               Build the $(BINARY) binary"
	@echo "  build-cli           Build the $(CLI_BINARY) binary"
	@echo "  build-wechat-login  Build the $(WECHAT_LOGIN_BINARY) binary"
	@echo "  install             Install all binaries to $$(go env GOPATH)/bin"
	@echo "  clean               Remove built binaries"
	@echo "  test                Run tests with race detector"
	@echo "  help                Show this help"
