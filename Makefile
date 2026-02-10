.PHONY: test build images

test:
	go test ./...

build:
	go build ./cmd/workspaces-operator
	go build ./cmd/capability-broker
	go build ./cmd/egress-proxy
	go build ./cmd/ws-proxy
	go build ./cmd/github-webhook
	go build ./cmd/wsctl
	go build ./cmd/agent-runner
	go build ./cmd/auditctl
	go build ./cmd/auditship

# Build container images locally (useful for dev clusters).
#
# Examples:
# - make images
# - make images REGISTRY=ghcr.io/<you> TAG=dev
REGISTRY ?= ghcr.io/haasonsaas
TAG ?= latest
DOCKER ?= docker

images:
	$(DOCKER) build -t $(REGISTRY)/workspaces-operator:$(TAG) -f images/workspaces-operator/Dockerfile .
	$(DOCKER) build -t $(REGISTRY)/workspaces-capability-broker:$(TAG) -f images/capability-broker/Dockerfile .
	$(DOCKER) build -t $(REGISTRY)/workspaces-egress-proxy:$(TAG) -f images/egress-proxy/Dockerfile .
	$(DOCKER) build -t $(REGISTRY)/workspaces-github-webhook:$(TAG) -f images/github-webhook/Dockerfile .
	$(DOCKER) build -t $(REGISTRY)/workspaces-agent-runner:$(TAG) -f images/agent-runner/Dockerfile .
	$(DOCKER) build -t $(REGISTRY)/workspaces-desktop:$(TAG) -f images/desktop/Dockerfile .
	$(DOCKER) build -t $(REGISTRY)/workspaces-auditship:$(TAG) -f images/auditship/Dockerfile .
	$(DOCKER) build -t $(REGISTRY)/workspaces-ws-proxy:$(TAG) -f images/ws-proxy/Dockerfile .
	$(DOCKER) build -t $(REGISTRY)/workspaces-ws-desktop-agent:$(TAG) -f images/ws-desktop-agent/Dockerfile .
	$(DOCKER) build -t $(REGISTRY)/workspaces-ws-relay:$(TAG) -f images/ws-relay/Dockerfile .
	$(DOCKER) build -t $(REGISTRY)/workspaces-ws-relayd:$(TAG) -f images/ws-relayd/Dockerfile .
