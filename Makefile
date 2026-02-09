.PHONY: test build

test:
	go test ./...

build:
	go build ./cmd/workspaces-operator
	go build ./cmd/capability-broker
	go build ./cmd/ws-proxy
