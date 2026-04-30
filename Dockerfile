# Stage 1: build the Go binary
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /agent-runner ./cmd/server

# Stage 2: runtime — node:alpine provides npm for agent CLI installation
FROM node:22-alpine
RUN apk add --no-cache git ca-certificates curl

# Install the agent CLI at image build time.
# AGENT_CLI=none (default) skips installation — use /install-cli at runtime.
# Pass AGENT_CLI_INSTALL_CMD to override the default install command for any CLI.
ARG AGENT_CLI=none
ARG AGENT_CLI_INSTALL_CMD

RUN if [ "$AGENT_CLI" = "none" ] || [ -z "$AGENT_CLI" ]; then \
      echo "skipping agent CLI install"; \
    elif [ -n "$AGENT_CLI_INSTALL_CMD" ]; then \
      sh -c "$AGENT_CLI_INSTALL_CMD"; \
    else \
      case "$AGENT_CLI" in \
        claude)   npm install -g @anthropic-ai/claude-code ;; \
        codex)    npm install -g @openai/codex ;; \
        opencode|opencode-ai) \
          ARCH=$(uname -m) && \
          URL=$(curl -fsSL https://api.github.com/repos/sst/opencode/releases/latest \
            | grep browser_download_url | grep linux | grep "$ARCH" | head -1 \
            | cut -d'"' -f4) && \
          curl -fsSL "$URL" -o /usr/local/bin/opencode && \
          chmod +x /usr/local/bin/opencode ;; \
        *) echo "no built-in install for '$AGENT_CLI'; pass AGENT_CLI_INSTALL_CMD" && exit 1 ;; \
      esac; \
    fi

COPY --from=builder /agent-runner /usr/local/bin/agent-runner

WORKDIR /app
EXPOSE 8080
ENTRYPOINT ["agent-runner"]
