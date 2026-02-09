FROM golang:1.25 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/workspaces-egress-proxy ./cmd/egress-proxy

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Match other components' non-root uid patterns.
RUN useradd -m -u 1000 -s /bin/bash proxy
USER 1000:1000

COPY --from=builder /out/workspaces-egress-proxy /usr/local/bin/workspaces-egress-proxy

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/workspaces-egress-proxy"]
