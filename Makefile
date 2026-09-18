.PHONY: test-area51

# Override these to use a different Linux host or checkout:
# make test-area51 REMOTE_HOST=example REMOTE_PROJECT_DIR=src/mcp-dap-server
REMOTE_HOST ?= area51
REMOTE_PROJECT_DIR ?= Code/mcp-dap-server
REMOTE_DELVE_DIR ?= Code/delve

# Sync the current working tree and run the complete race-enabled suite on
# Linux. Delve is installed from the existing remote checkout because it is
# required by the default-backend integration tests.
test-area51:
	rsync -az --exclude '.git' --exclude 'bin' --exclude 'debugprog' --exclude '*.dSYM' ./ $(REMOTE_HOST):$(REMOTE_PROJECT_DIR)/
	ssh $(REMOTE_HOST) 'set -eu; cd ~/"$(REMOTE_DELVE_DIR)"; go install ./cmd/dlv; export PATH="$$(go env GOPATH)/bin:$$PATH"; cd ~/"$(REMOTE_PROJECT_DIR)"; go test -race -v -count=1 ./...'
