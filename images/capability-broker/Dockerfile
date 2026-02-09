FROM golang:1.25 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/capability-broker ./cmd/capability-broker

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# Broker needs git for patch apply / PR creation.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      git \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 1000 -s /bin/bash broker
USER 1000:1000

COPY --from=builder /out/capability-broker /usr/local/bin/capability-broker

ENTRYPOINT ["/usr/local/bin/capability-broker"]
