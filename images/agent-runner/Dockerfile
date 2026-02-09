FROM golang:1.25 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/workspaces-agent-runner ./cmd/agent-runner

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      git \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/workspaces-agent-runner /usr/local/bin/workspaces-agent-runner

# Match AgentJob controller defaults.
RUN useradd -m -u 1000 -s /bin/bash agent
USER 1000:1000
WORKDIR /workspace
