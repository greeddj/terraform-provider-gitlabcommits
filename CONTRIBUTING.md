# Contributing

Thanks for taking the time to contribute! This document is intentionally
short — please read all of it.

## Quick start

```bash
git clone https://github.com/greeddj/terraform-provider-gitlabcommits
cd terraform-provider-gitlabcommits

just ci        # the full CI gate: check + lint + test + tf-fmt + examples + docs + headers + deps
just docs      # regenerate the docs (uses tfplugindocs)
just build     # local binary in ./dist (runs check + lint + test first)
```

If you don't have [Just](https://github.com/casey/just), each target maps to
a one-liner you can run manually — see the `Justfile`.

## Development loop

1. **Edit code** under `internal/provider/`. New code paths need unit tests in
   the same package — see `*_test.go` for examples (table-driven where it
   helps).
2. **Run `just ci`** before pushing - it is the exact gate CI runs:
   - `just check` (`go vet`, `staticcheck`, `govulncheck`, `fieldalignment`)
   - `just lint` (`golangci-lint`, including the `gofmt` formatter)
   - `just test` (`go test ./...`)
   - `terraform fmt -check` and `terraform validate` over `examples/`
   - generated-docs and copywrite-header sync checks
   - `go mod tidy -diff`
3. **Regenerate docs** if you changed any schema:
   `just docs` or `go generate ./...`. CI fails if `docs/` is out of sync.
4. **Use a conventional commit title** (`feat:`, `fix:`, `docs:`, `chore:`,
   `test:`) for any user-visible change - goreleaser builds the release
   changelog from commit titles; there is no CHANGELOG file to edit.

## Acceptance tests

Acceptance tests run against a real GitLab project. They are **not** part of
`just test` because they take secrets and quota. To run them locally:

```bash
export TF_ACC=1
export GITLAB_TOKEN='...'                  # api scope (see README Authentication)
export GITLAB_TEST_PROJECT_ID='you/sandbox'
export GITLAB_TEST_BRANCH='tf-acc-test'    # optional; must pre-exist unless GITLAB_TEST_BRANCH_FROM is set
export GITLAB_TEST_BRANCH_FROM='main'      # optional; create the branch from this ref, delete it after the test
go test -v -timeout=20m -run '^TestAcc' ./internal/...
```

In CI they run via `.github/workflows/acceptance.yml` (manual trigger /
nightly cron).

## Pull requests

- One change per PR. Refactors are welcome but please separate them from
  feature/bugfix work.
- Conventional commit style for the PR title (`feat:`, `fix:`, `docs:`,
  `chore:`, `test:`). Goreleaser uses these to produce the changelog.
- Keep PRs reviewable — under ~400 lines of diff is the sweet spot.
- New API surface (resources, data sources, schema attributes) needs a doc
  example in `examples/` so `tfplugindocs` picks it up.

## Code style

- Default to **no comments**. Names should carry their meaning. Add a comment
  only when *why* is non-obvious — invariants, workarounds, surprising
  behaviour.
- Don't write code for hypothetical future requirements.
- Validate at boundaries (user config, API responses); trust internal code.

## Filing an issue

Please include:

- provider version,
- Terraform version,
- the relevant HCL,
- expected vs actual behaviour,
- a minimal reproduction.
