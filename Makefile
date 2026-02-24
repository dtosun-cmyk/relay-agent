.PHONY: build clean test run lint bench install help release release-upload deploy

BINARY_NAME=relay-agent
BINARY_ASSET=relay-agent-linux-amd64
BUILD_DIR=bin
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS=-ldflags "-s -w -X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"
GITHUB_REPO=dtosun-cmyk/relay-agent

# Build for production (Linux AMD64, statically linked)
build:
	@echo "Building $(BINARY_NAME) $(VERSION) for Linux AMD64..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/relay-agent
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

# Build for local development (native platform)
build-local:
	@echo "Building $(BINARY_NAME) $(VERSION) for local platform..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/relay-agent
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(BUILD_DIR)
	@echo "Clean complete"

# Run tests with race detector
test:
	@echo "Running tests with race detector..."
	go test -v -race ./...

# Run tests with coverage
test-coverage:
	@echo "Running tests with coverage..."
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Run the application locally
run:
	@echo "Running $(BINARY_NAME)..."
	go run ./cmd/relay-agent -config config/config.yaml

# Run linter (requires golangci-lint)
lint:
	@echo "Running linter..."
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed. Install: https://golangci-lint.run/usage/install/"; exit 1; }
	golangci-lint run

# Run benchmarks
bench:
	@echo "Running benchmarks..."
	go test -bench=. -benchmem ./internal/parser/

# Install to /opt/relay-agent/bin
install:
	@echo "Installing $(BINARY_NAME) to /opt/relay-agent/bin/..."
	@mkdir -p /opt/relay-agent/bin
	cp $(BUILD_DIR)/$(BINARY_NAME) /opt/relay-agent/bin/
	chmod +x /opt/relay-agent/bin/$(BINARY_NAME)
	@echo "Installation complete"

# Format code
fmt:
	@echo "Formatting code..."
	go fmt ./...
	@echo "Format complete"

# Tidy dependencies
tidy:
	@echo "Tidying dependencies..."
	go mod tidy
	@echo "Tidy complete"

# Verify dependencies
verify:
	@echo "Verifying dependencies..."
	go mod verify
	@echo "Verify complete"

# Download dependencies
deps:
	@echo "Downloading dependencies..."
	go mod download
	@echo "Dependencies downloaded"

# Create a new release: make release TAG=v1.0.1
release: build
	@if [ -z "$(TAG)" ]; then echo "Usage: make release TAG=v1.1.0"; exit 1; fi
	@echo "Creating release $(TAG)..."
	git tag -a $(TAG) -m "Release $(TAG)"
	git push origin $(TAG)
	@echo "Creating GitHub release..."
	@RELEASE_ID=$$(curl -s -X POST \
		-H "Authorization: token $${GITHUB_TOKEN}" \
		-H "Accept: application/vnd.github.v3+json" \
		"https://api.github.com/repos/$(GITHUB_REPO)/releases" \
		-d '{"tag_name":"$(TAG)","name":"$(TAG)","body":"Release $(TAG)","draft":false,"prerelease":false}' \
		| python3 -c "import sys,json; print(json.load(sys.stdin)['id'])"); \
	echo "Uploading binary (release ID: $$RELEASE_ID)..."; \
	curl -s -L -X POST \
		-H "Authorization: token $${GITHUB_TOKEN}" \
		-H "Content-Type: application/octet-stream" \
		"https://uploads.github.com/repos/$(GITHUB_REPO)/releases/$$RELEASE_ID/assets?name=$(BINARY_ASSET)" \
		--data-binary @$(BUILD_DIR)/$(BINARY_NAME) > /dev/null
	@echo "Release $(TAG) created: https://github.com/$(GITHUB_REPO)/releases/tag/$(TAG)"

# Upload binary to existing release: make release-upload TAG=v1.0.0
release-upload: build
	@if [ -z "$(TAG)" ]; then echo "Usage: make release-upload TAG=v1.0.0"; exit 1; fi
	@echo "Uploading binary to release $(TAG)..."
	@RELEASE_ID=$$(curl -s \
		-H "Authorization: token $${GITHUB_TOKEN}" \
		"https://api.github.com/repos/$(GITHUB_REPO)/releases/tags/$(TAG)" \
		| python3 -c "import sys,json; print(json.load(sys.stdin)['id'])"); \
	OLD_ASSET=$$(curl -s \
		-H "Authorization: token $${GITHUB_TOKEN}" \
		"https://api.github.com/repos/$(GITHUB_REPO)/releases/$$RELEASE_ID/assets" \
		| python3 -c "import sys,json; assets=[a for a in json.load(sys.stdin) if a['name']=='$(BINARY_ASSET)']; print(assets[0]['id'] if assets else '')" 2>/dev/null); \
	if [ -n "$$OLD_ASSET" ]; then \
		echo "Deleting old binary asset..."; \
		curl -s -X DELETE \
			-H "Authorization: token $${GITHUB_TOKEN}" \
			"https://api.github.com/repos/$(GITHUB_REPO)/releases/assets/$$OLD_ASSET" > /dev/null; \
	fi; \
	echo "Uploading new binary..."; \
	curl -s -L -X POST \
		-H "Authorization: token $${GITHUB_TOKEN}" \
		-H "Content-Type: application/octet-stream" \
		"https://uploads.github.com/repos/$(GITHUB_REPO)/releases/$$RELEASE_ID/assets?name=$(BINARY_ASSET)" \
		--data-binary @$(BUILD_DIR)/$(BINARY_NAME) > /dev/null
	@echo "Binary uploaded to $(TAG)"

# Deploy: build, upload to current tag, restart service
deploy: build
	@echo "Deploying $(VERSION)..."
	cp $(BUILD_DIR)/$(BINARY_NAME) /opt/relay-agent/bin/$(BINARY_NAME)
	chmod +x /opt/relay-agent/bin/$(BINARY_NAME)
	systemctl restart relay-agent
	@sleep 2
	@systemctl is-active --quiet relay-agent && echo "Deploy complete - relay-agent is running" || echo "WARNING: relay-agent failed to start"

# Show help
help:
	@echo "Available targets:"
	@echo "  build           - Build for production (Linux AMD64)"
	@echo "  build-local     - Build for local platform"
	@echo "  clean           - Remove build artifacts"
	@echo "  test            - Run tests with race detector"
	@echo "  test-coverage   - Run tests with coverage report"
	@echo "  run             - Run application locally"
	@echo "  lint            - Run golangci-lint"
	@echo "  bench           - Run benchmarks"
	@echo "  install         - Install binary to /opt/relay-agent/bin"
	@echo "  fmt             - Format code"
	@echo "  tidy            - Tidy dependencies"
	@echo "  verify          - Verify dependencies"
	@echo "  deps            - Download dependencies"
	@echo "  release TAG=x   - Build, tag, create GitHub release with binary"
	@echo "  release-upload TAG=x - Re-upload binary to existing release"
	@echo "  deploy          - Build, install, restart service"
	@echo "  help            - Show this help message"

.DEFAULT_GOAL := build
