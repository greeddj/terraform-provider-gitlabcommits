# Terraform Provider GitLab Commits - Justfile

# Variables
binary := "terraform-provider-gitlabcommits"
version := `git describe --tags --abbrev=0 2>/dev/null || echo "dev"`
PKG := "github.com/greeddj/{{binary}}"
flags := "-s -w -extldflags '-static' -X {{PKG}}/main.version={{version}}"

# Default recipe (runs when you just type 'just')
default:
  @just --list

# Install necessary development tools
tools:
	brew install golangci/tap/golangci-lint
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest

# Run linters
lint:
	golangci-lint run ./...

# Run code vetting and vulnerability checks
check: deps
	go vet -mod vendor ./...
	staticcheck ./...
	govulncheck ./...

# Build the provider
build:
  mkdir -p dist
  CGO_ENABLED=0 go build -mod vendor -ldflags="{{flags}}" -o dist/{{binary}} main.go

# Run tests
test:
  go test -v ./...

# Refresh Go module dependencies
deps:
  go mod tidy
  go mod vendor
