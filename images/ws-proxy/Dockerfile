FROM golang:1.25 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/ws-proxy ./cmd/ws-proxy

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 1000 -s /usr/sbin/nologin wsproxy
USER 1000:1000

COPY --from=builder /out/ws-proxy /usr/local/bin/ws-proxy

ENTRYPOINT ["/usr/local/bin/ws-proxy"]
