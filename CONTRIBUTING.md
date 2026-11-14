# Contributing

For a bug report or a feature request, use the issue templates. For a security problem, do not open a public issue. Use the procedure in [SECURITY.md](SECURITY.md).

## Requirements

Put these tools on `PATH`:

- Go 1.27.4 or later. `go.mod` sets this version.
- [`just`](https://github.com/casey/just).
- `golangci-lint` v2, built with Go 1.27 or later. CI uses v2.15.0.
- The `terraform` CLI. CI uses Terraform 1.17.2.
- `git`.

The other tools run through `go tool`, so you do not install them.

## Development loop

1. Change the code in `internal/provider/`.
2. Add unit tests in the same package.
3. If you changed a source of `docs/`, run `just docs`. See [Documentation](#documentation).
4. Commit the change and the changes in `docs/` with a conventional title. See [Commit titles](#commit-titles).
5. Run `just ci`.

To use a local build with Terraform:

1. Run `just build`.
2. In your Terraform CLI configuration, add a `dev_overrides` block for `greeddj/gitlabcommits` with the absolute path of `dist/`. `scripts/validate-examples.sh` shows an example.

To attach a debugger such as delve, start the provider binary with the `-debug` flag.

## The just targets

| Target | What it does |
| --- | --- |
| `just ci` | Runs `check`, `lint`, `test`, `check-tf-fmt`, `check-examples`, `docs-check`, `headers-check` and `check-deps`. |
| `just check` | Runs `go vet`, `staticcheck`, `govulncheck` and `fieldalignment`. |
| `just test` | Runs `go test -race ./...`. |
| `just build` | Runs `check`, `lint` and `test`, then writes a static binary to `dist/`. |
| `just docs`, `just docs-check` | Write `docs/` again. `docs-check` then fails if `git status` shows a change in `docs/`. |
| `just tf-fmt`, `just headers`, `just deps`, `just fix` | Repair what `check-tf-fmt`, `headers-check`, `check-deps` and `fieldalignment` find. `just fix` also runs `go fix`. |

`govulncheck` needs network access. A new vulnerability can make it fail without a change to the code. CI also does a test release with goreleaser, which `just ci` does not do.

## Acceptance tests

The acceptance tests (`TestAcc*`) run against a real GitLab project. They commit to it, and they delete files and branches.

**Caution:** Do not export `TF_ACC` in the shell where you run `just test`, `just build` or `just ci`. When `TF_ACC` has a value, these targets also run the acceptance tests. Their 10-minute `go test` timeout can stop the tests before they remove their files.

### Environment variables

- `TF_ACC` (necessary): a value that is not empty starts the tests.
- `GITLAB_TOKEN` (necessary): a token with the `api` scope for the test project.
- `GITLAB_TEST_PROJECT_ID` (necessary): the project path, for example `you/sandbox` (not URL-encoded), or the numeric ID.
- `GITLAB_TEST_BRANCH`: the test branch, by default `tf-acc-test`. Without `GITLAB_TEST_BRANCH_FROM`, this branch must exist, and the tests leave their commits in it.
- `GITLAB_TEST_BRANCH_FROM`: a branch name or a full commit SHA of 40 or 64 characters, not a tag. The provider creates a missing test branch from it. Each test deletes the test branch when it ends, also a branch that existed before.
- `GITLAB_BASE_URL`: the URL of your GitLab instance. Without it, the tests use `https://gitlab.com` and send `GITLAB_TOKEN` there.
- `TF_ACC_TERRAFORM_PATH`: the path of the `terraform` binary for the tests.
- `TF_ACC_TERRAFORM_VERSION`: a Terraform version that the tests download when `TF_ACC_TERRAFORM_PATH` is empty.

**Caution:** Do not set `GITLAB_TEST_BRANCH` to a branch that you need while you use `GITLAB_TEST_BRANCH_FROM`.

A fine-grained token with only the [permissions of the provider](docs/index.md#authentication) cannot delete the test branch. With such a token, delete it yourself after a run with `GITLAB_TEST_BRANCH_FROM`.

### Run the acceptance tests

1. Set the variables for your test project:

   ```bash
   export GITLAB_TOKEN='...'
   export GITLAB_TEST_PROJECT_ID='you/sandbox'
   export GITLAB_BASE_URL='https://gitlab.example.com'  # self-hosted GitLab only
   export GITLAB_TEST_BRANCH_FROM='main'                # optional
   ```

2. Run the tests. Set `TF_ACC=1` on the command only:

   ```bash
   TF_ACC=1 go test -count=1 -v -timeout=20m -run '^TestAcc' ./internal/...
   ```

Keep `-count=1`. Without it, `go test` can show a cached pass.

If `go test` stops at its timeout, the tests do not remove their files. Then clean the test project yourself:

- If you use `GITLAB_TEST_BRANCH_FROM`, delete the test branch.
- If not, delete the files under `tf-acc-test/` on the test branch.

### Acceptance tests in CI

`.github/workflows/acceptance.yml` needs the repository secrets `GITLAB_TEST_TOKEN` and `GITLAB_TEST_PROJECT_ID`, plus `GITLAB_TEST_BASE_URL` for self-hosted GitLab. Without a necessary secret, a manual run fails, but a scheduled run or a push run passes with a warning and no tests.

Each run uses its own test branch from `main` and deletes it. In a fork, start the workflow manually.

## Documentation

Write documentation for people in [ASD-STE100](https://www.asd-ste100.org/).

`just docs` writes `docs/` from the schema in `internal/provider/`, the templates in `templates/`, `examples/provider/provider.tf`, `resource*.tf` and `import.sh` in `examples/resources/<name>/`, and `data-source*.tf` in `examples/data-sources/<name>/`. Do not edit `docs/`.

Show a new resource, data source or attribute in an example that `just docs` reads. `examples/complete` and `examples/for_each` do not go into `docs/`.

## Commit titles

goreleaser makes the release notes from the commit titles. They do not show titles that start with `chore` or `test:`, merge commits, or titles that contain `merge conflict` or `go mod tidy`. The prefix of each other title sets its section:

- Features, Bug Fixes, Documentation: `feat:`, `fix:`, `docs:`.
- Dependency Updates, CI: `fix(deps):`, `fix(ci):`.
- Other Work: all other titles, for example with `refactor:` or with no prefix.

Use `feat:`, `fix:` or `docs:` for a change that users can see.

## Pull requests

- Put one change in each pull request (PR). Keep a refactoring in a separate PR.
- Keep each PR small, about 400 changed lines or fewer.
- Give the PR a conventional title. With a squash merge, make sure that the squash commit has this title.
- With a merge commit, give each commit in the branch a conventional title.

## Code style

- Use names that tell what the code does.
- Write a comment only when the reason for the code is not clear.
- Do not write code for possible future requirements.
- Validate user configuration and API responses. Trust the internal code.
