##@ General

.PHONY: help
help: ## Display this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Format all Go source files
	gofmt -w .

.PHONY: vet
vet: ## Run go vet against code
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: tidy
tidy: ## Tidy go module dependencies
	go mod tidy

##@ Build

.PHONY: fetch-ui
fetch-ui: ## Download the Scalar API reference JS bundle for disconnected (air-gapped) deployments
	@mkdir -p internal/server/ui
	@echo "Downloading Scalar API reference bundle..."
	curl -fsSL \
	  "https://cdn.jsdelivr.net/npm/@scalar/api-reference" \
	  -o internal/server/ui/scalar.min.js
	@echo "Saved to internal/server/ui/scalar.min.js ($(shell wc -c < internal/server/ui/scalar.min.js) bytes)"

.PHONY: build
build: fmt vet ## Build the server binary
	go build -o bin/server ./cmd/server/...

.PHONY: run
run: fmt vet ## Run the server locally using the default config file
	go run ./cmd/server/... --config=config/config.yaml

##@ Test

.PHONY: test
test: ## Run all unit tests with race detection
	go test -v -race ./...

.PHONY: test-cover
test-cover: ## Run tests and generate an HTML coverage report
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report written to coverage.html"

##@ Docker

.PHONY: docker-build
docker-build: ## Build the Docker image
	docker build -t ghcr.io/dana-team/capp-backend:latest .

.PHONY: docker-push
docker-push: ## Push the Docker image to the registry
	docker push ghcr.io/dana-team/capp-backend:latest

##@ Deployment

.PHONY: deploy
deploy: ## Apply Kubernetes manifests to the current cluster
	kubectl apply -f deploy/
