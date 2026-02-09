FROM golang:1.25 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/workspaces-operator ./cmd/workspaces-operator

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 1000 -s /usr/sbin/nologin operator
USER 1000:1000

COPY --from=builder /out/workspaces-operator /usr/local/bin/workspaces-operator

ENTRYPOINT ["/usr/local/bin/workspaces-operator"]
