FROM golang:1.23 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/workspaces-auditship ./cmd/auditship

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/workspaces-auditship /usr/local/bin/workspaces-auditship

RUN useradd -m -u 1000 -s /usr/sbin/nologin auditship
USER 1000:1000

ENTRYPOINT ["/usr/local/bin/workspaces-auditship"]

