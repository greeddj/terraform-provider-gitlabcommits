# Terraform Provider GitLab Commits - Justfile

set shell := ["bash", "-cu"]

binary := "terraform-provider-gitlabcommits"
version := `git describe --tags --abbrev=0 2>/dev/null || echo "dev"`

default:
	@just --list

tf-fmt:
	terraform fmt -recursive examples/
check-tf-fmt:
	terraform fmt -check -recursive examples/

test:
	mkdir -p .gocache
	go test -v ./...

lint:
	mkdir -p .gocache .gomodcache .staticcheck .golangci-lint-cache
	go vet ./...
	go tool staticcheck ./...
	golangci-lint run ./...

docs:
	mkdir -p .gocache .gomodcache
	go generate ./...

docs-check:
	just docs
	git diff --exit-code -- docs

check-vet:
	go vet ./...

check-staticcheck:
	go tool staticcheck ./...

check-govulncheck:
	go tool govulncheck ./...

check-fieldalignment:
	go tool fieldalignment ./...

fix:
	go fix ./...
	go tool fieldalignment -fix ./...


build:
	mkdir -p dist
	mkdir -p .gocache
	CGO_ENABLED=0 go build -ldflags="-s -w -extldflags '-static' -X main.version={{version}}" -o dist/{{binary}} main.go

headers:
	mkdir -p .gocache .gomodcache
	go tool copywrite headers -d . --config .copywrite.hcl

headers-check:
	just headers
	git diff --exit-code

security:
	mkdir -p .gocache .gomodcache
	go tool govulncheck ./...

deps:
	mkdir -p .gocache .gomodcache
	go mod tidy

check: check-tf-fmt check-vet check-staticcheck check-govulncheck check-fieldalignment

ci:
	just check
	just docs-check
	just headers-check
