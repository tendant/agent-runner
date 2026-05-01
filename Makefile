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
        docker-build docker-push docker-release docker-release-multiarch release help

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

release:
	@LAST=$$(cat VERSION | tr -d '[:space:]'); \
	MAJOR=$$(echo $$LAST | cut -d. -f1 | tr -d v); \
	MINOR=$$(echo $$LAST | cut -d. -f2); \
	PATCH=$$(echo $$LAST | cut -d. -f3); \
	NEXT=v$$MAJOR.$$MINOR.$$((PATCH+1)); \
	echo $$NEXT > VERSION; \
	echo "Releasing $$NEXT → $(IMAGE)"; \
	docker buildx build --platform linux/amd64,linux/arm64 \
	  --build-arg AGENT_CLI=$(DOCKER_CLI) $(_INSTALL_CMD_ARG) \
	  -t $(IMAGE):$$NEXT -t $(IMAGE):latest --push .

clean:
	rm -f $(BINARY) $(CLI_BINARY) $(WECHAT_LOGIN_BINARY)

test:
	go test -race ./...

help:
	@echo "Usage: make [target] [VAR=value ...]"
	@echo ""
	@echo "Development:"
	@echo "  build               Build the server binary"
	@echo "  build-cli           Build the CLI binary"
	@echo "  build-wechat-login  Build the wechat-login binary"
	@echo "  install             Install all binaries to \$$(go env GOPATH)/bin"
	@echo "  test                Run tests with race detector"
	@echo "  clean               Remove built binaries"
	@echo ""
	@echo "Agent CLI:"
	@echo "  setup-cli           Install agent CLI locally (AGENT_CLI=opencode|claude|codex)"
	@echo ""
	@echo "Docker release:"
	@echo "  docker-build        Build image  IMAGE=<registry/name> TAG=<tag>"
	@echo "                        DOCKER_CLI=opencode|claude|codex  bake in a CLI (default: none)"
	@echo "  docker-push         Push image   IMAGE=<registry/name> TAG=<tag>"
	@echo "  docker-release      Build + push (single arch)"
	@echo "  docker-release-multiarch  Build + push linux/amd64 and linux/arm64"
	@echo "  release             Auto-increment patch, git tag, push versioned + latest"
	@echo ""
	@echo "  Examples:"
	@echo "    make release IMAGE=wang/agent-runner"
	@echo "    make release IMAGE=wang/agent-runner DOCKER_CLI=opencode"
	@echo "    make docker-release IMAGE=ghcr.io/myorg/agent-runner TAG=v1.2.3 DOCKER_CLI=opencode"
	@echo "    make docker-release-multiarch IMAGE=ghcr.io/myorg/agent-runner TAG=latest"
	@echo ""
	@echo "  help                Show this help"
