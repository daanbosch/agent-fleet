API_BINARY := fastvm-api
CLI_BINARY := fastvm
ENROLLER_BINARY := fastvm-enroller
API_PKG    := github.com/team-telnyx/fast-vm-spinner/cmd/fastvm-api
CLI_PKG    := github.com/team-telnyx/fast-vm-spinner/cmd/fastvm-cli
ENROLLER_PKG := github.com/team-telnyx/fast-vm-spinner/cmd/fastvm-enroller
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -ldflags "-s -w -X main.version=$(VERSION)"
REGISTRY   ?= registry.internal.telnyx.com
IMAGE_NAMESPACE ?= infra
ENROLLER_IMAGE_NAME ?= fastvm-enroller-oci
ENROLLER_IMAGE_VERSION ?= $(VERSION)
ENROLLER_IMAGE := $(REGISTRY)/$(IMAGE_NAMESPACE)/$(ENROLLER_IMAGE_NAME):$(ENROLLER_IMAGE_VERSION)
ENROLLER_RED_IMAGE := $(REGISTRY)/$(IMAGE_NAMESPACE)/$(ENROLLER_IMAGE_NAME):red

# Local install paths
LOCAL_BIN  ?= /usr/local/bin
MAN_DIR    ?= /usr/local/share/man/man1

# Remote deploy target
DEPLOY_HOST  ?= $(FASTVM_HOST)
DEPLOY_USER  ?= terraform
DEPLOY_KEY   ?= $(FASTVM_SSH_KEY)
REMOTE_BIN   ?= /usr/local/bin
DEPLOY_BACKEND ?= firecracker
DEPLOY_SSH_HOST ?= $(DEPLOY_HOST)
FIRECRACKER_INSTALL_DIR ?= /opt/fastvm-microvm
FIRECRACKER_DEFAULT_TEMPLATE ?= 9013
FIRECRACKER_NODE ?= $(DEPLOY_SSH_HOST)
FIRECRACKER_SSH_USER ?= root

.PHONY: build build-api build-cli build-enroller build-enroller-image test-enroller-image test clean install install-man deploy setup-host help

## build: Build CLI, API, and enrollment service binaries
build: build-cli build-api build-enroller

## build-cli: Build the CLI for the local machine
build-cli:
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(CLI_BINARY) $(CLI_PKG)

## build-api: Cross-compile the API server for Linux amd64
build-api:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(API_BINARY) $(API_PKG)

## build-enroller: Cross-compile the FastVM enrollment service for Linux amd64
build-enroller:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(ENROLLER_BINARY) $(ENROLLER_PKG)

## build-enroller-image: Build the FastVM enrollment service container image
build-enroller-image:
	docker build -f Dockerfile.enroller -t $(ENROLLER_IMAGE) -t $(ENROLLER_RED_IMAGE) .

## test-enroller-image: Build and smoke-test the FastVM enrollment service image
test-enroller-image: build-enroller-image
	docker run --rm $(ENROLLER_IMAGE) --help >/dev/null

## test: Run tests
test:
	go test ./...

## clean: Remove build artifacts
clean:
	rm -f $(API_BINARY) $(CLI_BINARY) $(ENROLLER_BINARY)

## install: Install CLI + man pages locally (your machine)
install: build-cli install-man
	sudo cp $(CLI_BINARY) $(LOCAL_BIN)/$(CLI_BINARY)
	sudo chmod +x $(LOCAL_BIN)/$(CLI_BINARY)
	@echo "Installed $(CLI_BINARY) to $(LOCAL_BIN)"
	@echo "Set FASTVM_API to point to your FastVM API host"

## install-man: Install man pages locally
install-man:
	@sudo mkdir -p $(MAN_DIR) 2>/dev/null || true
	sudo cp docs/fastvm.1 $(MAN_DIR)/fastvm.1
	sudo cp docs/fastvm-api.1 $(MAN_DIR)/fastvm-api.1
	@echo "Man pages installed. Try: man fastvm"

## deploy: Build, deploy, and restart the API server on the backend host
deploy: build-api
	@if [ -z "$(DEPLOY_HOST)" ]; then \
		echo "Error: set FASTVM_HOST or DEPLOY_HOST"; \
		echo "  make deploy DEPLOY_HOST=atl1-pc1-025 DEPLOY_KEY=~/.ssh/terraform_rsa"; \
		exit 1; \
	fi
	@echo "Deploying to $(DEPLOY_USER)@$(DEPLOY_HOST)..."
	scp $(if $(DEPLOY_KEY),-i $(DEPLOY_KEY)) \
		$(API_BINARY) \
		$(DEPLOY_USER)@$(DEPLOY_HOST):/tmp/
	ssh $(if $(DEPLOY_KEY),-i $(DEPLOY_KEY)) \
		$(DEPLOY_USER)@$(DEPLOY_HOST) " \
		sudo mv /tmp/$(API_BINARY) $(REMOTE_BIN)/$(API_BINARY) && \
		sudo chmod +x $(REMOTE_BIN)/$(API_BINARY) && \
		sudo systemctl restart fastvm-api && \
		echo 'Deployed and restarted fastvm-api on $(DEPLOY_HOST)'"

## setup-host: Install systemd service and env config on the backend host (one-time)
setup-host:
	@if [ -z "$(DEPLOY_HOST)" ]; then \
		echo "Error: set FASTVM_HOST or DEPLOY_HOST"; exit 1; \
	fi
	@if [ -z "$(UPDATE_SECRET)" ]; then \
		echo "Error: set UPDATE_SECRET for the self-update endpoint"; exit 1; \
	fi
	@echo "Setting up fastvm-api service on $(DEPLOY_HOST)..."
	scp $(if $(DEPLOY_KEY),-i $(DEPLOY_KEY)) \
		scripts/fastvm-api.service \
		$(DEPLOY_USER)@$(DEPLOY_HOST):/tmp/
	ssh $(if $(DEPLOY_KEY),-i $(DEPLOY_KEY)) \
		$(DEPLOY_USER)@$(DEPLOY_HOST) " \
		sudo mv /tmp/fastvm-api.service /etc/systemd/system/fastvm-api.service && \
		echo 'FASTVM_BACKEND=$(DEPLOY_BACKEND)' | sudo tee /etc/default/fastvm-api > /dev/null && \
		echo 'FASTVM_SSH_HOST=$(DEPLOY_SSH_HOST)' | sudo tee -a /etc/default/fastvm-api > /dev/null && \
		echo 'FASTVM_UPDATE_SECRET=$(UPDATE_SECRET)' | sudo tee -a /etc/default/fastvm-api > /dev/null && \
		if [ -n '$(GITHUB_TOKEN)' ]; then echo 'GITHUB_TOKEN=$(GITHUB_TOKEN)' | sudo tee -a /etc/default/fastvm-api > /dev/null; fi && \
		if [ '$(DEPLOY_BACKEND)' = 'firecracker' ]; then \
			echo 'FASTVM_FIRECRACKER_INSTALL_DIR=$(FIRECRACKER_INSTALL_DIR)' | sudo tee -a /etc/default/fastvm-api > /dev/null && \
			echo 'FASTVM_FIRECRACKER_DEFAULT_TEMPLATE=$(FIRECRACKER_DEFAULT_TEMPLATE)' | sudo tee -a /etc/default/fastvm-api > /dev/null && \
			echo 'FASTVM_FIRECRACKER_NODE=$(FIRECRACKER_NODE)' | sudo tee -a /etc/default/fastvm-api > /dev/null && \
			echo 'FASTVM_FIRECRACKER_SSH_USER=$(FIRECRACKER_SSH_USER)' | sudo tee -a /etc/default/fastvm-api > /dev/null; \
		fi && \
		sudo chmod 600 /etc/default/fastvm-api && \
		sudo systemctl daemon-reload && \
		sudo systemctl enable fastvm-api && \
		echo 'Service installed. Run: make deploy to deploy the binary.'"

## help: Show available targets
help:
	@grep -E '^## ' Makefile | sed 's/## //' | column -t -s ':'
