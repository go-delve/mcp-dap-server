.PHONY: build test test-integration

build:
	go build -o bin/mcp-dap-server .

test:
	go test -race -count=1 ./...

# test-integration: run E2E integration tests (tagged //go:build integration).
# TestMain handles docker-compose up/down; the preflight check below ensures
# the Docker daemon is reachable before running tests.
# First run may take 2-5 min due to image pulls. Pre-pull to avoid timeouts:
#   docker pull golang:1.26 && docker pull alpine/socat
# Note: the dlv binary is bind-mounted from the host ($HOME/go2/bin/dlv).
#       Adjust the mount path in testdata/docker-compose.yml if needed.
test-integration:
	docker info > /dev/null
	go test -v -race -tags=integration -timeout=300s -skip 'TestGDB' ./...
