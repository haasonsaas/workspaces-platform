FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      git \
    && rm -rf /var/lib/apt/lists/*

# Match AgentJob controller defaults.
RUN useradd -m -u 1000 -s /bin/bash agent
USER 1000:1000
WORKDIR /workspace

