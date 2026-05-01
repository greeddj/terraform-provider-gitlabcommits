PROJECT := "terraform-provider-gitlabcommits"
VERSION := `sh -c 'git describe --tags --abbrev=0 2>/dev/null || git rev-parse --abbrev-ref HEAD'`
LDFLAGS := "-s -w -X main.version=" + VERSION

deps:
	@echo "===== Check deps for {{PROJECT}} ====="
	go mod tidy

lint:
	@echo "===== Lint {{PROJECT}} ====="
	golangci-lint run ./... --timeout=5m

test:
	@echo "===== Test {{PROJECT}} ====="
	go test ./...

check:
	@echo "===== Check {{PROJECT}} ====="
	go vet ./...
	go tool staticcheck ./...
	go tool govulncheck ./...
	go tool fieldalignment ./...

fix:
	@echo "===== Fix {{PROJECT}} ====="
	go fix ./...
	go tool fieldalignment -fix ./...

tf-fmt:
	@echo "===== Format Terraform examples ====="
	terraform fmt -recursive examples/

check-tf-fmt:
	@echo "===== Check Terraform fmt ====="
	terraform fmt -check -recursive examples/

docs:
	@echo "===== Regenerate provider docs ====="
	go generate ./...

docs-check: docs
	@echo "===== Verify generated docs are in sync ====="
	git diff --exit-code -- docs

headers:
	@echo "===== Apply copywrite headers ====="
	go tool copywrite headers -d . --config .copywrite.hcl

headers-check: headers
	@echo "===== Verify copywrite headers are in sync ====="
	git diff --exit-code

build: check lint test
	@echo "===== Build {{PROJECT}} ====="
	mkdir -p dist
	test -f dist/{{PROJECT}} && rm -f dist/{{PROJECT}} || echo "Not exist dist/{{PROJECT}}"
	CGO_ENABLED=0 go build -trimpath -ldflags="{{LDFLAGS}}" -o ./dist/{{PROJECT}} main.go

ci: check lint test check-tf-fmt docs-check headers-check
	@echo "===== CI gate passed for {{PROJECT}} ====="
