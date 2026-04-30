BINARY := agent-runner
CLI_BINARY := agent-cli
WECHAT_LOGIN_BINARY := wechat-login
CMD := ./cmd/server
CLI_CMD := ./cmd/cli
WECHAT_LOGIN_CMD := ./cmd/wechat-login
PREFIX := $(shell go env GOPATH)
AGENT_CLI ?= opencode   # used by setup-cli (local install)

IMAGE                ?= agent-runner
TAG                  ?= latest
DOCKER_CLI           ?= none    # agent CLI baked into the Docker image; none = skip
AGENT_CLI_INSTALL_CMD ?=

# Internal: add --build-arg AGENT_CLI_INSTALL_CMD only when set
_INSTALL_CMD_ARG = $(if $(AGENT_CLI_INSTALL_CMD),--build-arg AGENT_CLI_INSTALL_CMD="$(AGENT_CLI_INSTALL_CMD)",)

.DEFAULT_GOAL := help
.PHONY: build build-cli build-wechat-login install clean test setup-cli \
        docker-build docker-push docker-release docker-release-multiarch help

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

setup-cli:
	@case "$(AGENT_CLI)" in \
	  opencode) \
	    ARCH=$$(uname -m) && \
	    URL=$$(curl -fsSL https://api.github.com/repos/sst/opencode/releases/latest \
	      | grep browser_download_url | grep linux | grep "$$ARCH" | head -1 \
	      | cut -d'"' -f4) && \
	    curl -fsSL "$$URL" -o /usr/local/bin/opencode && \
	    chmod +x /usr/local/bin/opencode ;; \
	  codex)    npm install -g @openai/codex ;; \
	  claude|*) npm install -g @anthropic-ai/claude-code ;; \
	esac

docker-build:
	docker build --build-arg AGENT_CLI=$(DOCKER_CLI) $(_INSTALL_CMD_ARG) -t $(IMAGE):$(TAG) .

docker-push:
	docker push $(IMAGE):$(TAG)

docker-release: docker-build docker-push

docker-release-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64 \
		--build-arg AGENT_CLI=$(DOCKER_CLI) $(_INSTALL_CMD_ARG) \
		-t $(IMAGE):$(TAG) --push .

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
	@echo "  setup-cli           Install the agent CLI (AGENT_CLI=opencode|claude|codex)"
	@echo "  docker-build        Build Docker image (no CLI by default)"
	@echo "                      DOCKER_CLI=opencode|claude|codex to bake in a CLI"
	@echo "  docker-push         Push Docker image"
	@echo "  docker-release      Build and push Docker image"
	@echo "  docker-release-multiarch  Build and push multi-arch image (amd64+arm64)"
	@echo "  clean               Remove built binaries"
	@echo "  test                Run tests with race detector"
	@echo "  help                Show this help"
