---
name: gitlab-api-docs
description: How to fetch GitLab documentation in LLM-optimized markdown form. Every docs.gitlab.com page exposes a clean .md variant at `<page>/index.md` (no HTML chrome, full content, content-type text/markdown). Load this skill whenever you need to verify a GitLab REST API contract, look up an endpoint, check rate-limit semantics, or audit auth scope requirements.
---

# GitLab docs — LLM-optimized URLs

`docs.gitlab.com` ships an LLM-friendly version of every page. The website renders a "Copy for LLM" button; under the hood it just serves the same markdown source at a predictable URL.

## URL pattern

| HTML page (browser) | LLM markdown (WebFetch) |
| --- | --- |
| `https://docs.gitlab.com/api/commits/` | `https://docs.gitlab.com/api/commits/index.md` |
| `https://docs.gitlab.com/api/repository_files/` | `https://docs.gitlab.com/api/repository_files/index.md` |
| `https://docs.gitlab.com/api/branches/` | `https://docs.gitlab.com/api/branches/index.md` |
| `https://docs.gitlab.com/api/rest/authentication/` | `https://docs.gitlab.com/api/rest/authentication/index.md` |

**Rule:** take the page URL, ensure trailing `/`, append `index.md`. Returns `content-type: text/markdown; charset=utf-8`.

## Site-wide index

`https://docs.gitlab.com/llms.txt` lists the full navigation tree with absolute URLs to every section. Useful to discover what exists before fetching a specific page.

## Endpoints we care about (this provider)

Hot paths in `internal/provider/files_resource.go` map to:

| Code | GitLab endpoint | LLM doc URL |
| --- | --- | --- |
| `RepositoryFiles.GetFileMetaData` (HEAD-style drift probe) | `HEAD /projects/:id/repository/files/:file_path` | <https://docs.gitlab.com/api/repository_files/index.md> |
| `RepositoryFiles.GetFile` (full body on drift) | `GET /projects/:id/repository/files/:file_path` | <https://docs.gitlab.com/api/repository_files/index.md> |
| `Commits.CreateCommit` (the single per-apply commit) | `POST /projects/:id/repository/commits` | <https://docs.gitlab.com/api/commits/index.md> |
| `Branches.GetBranch` (branch_head data source) | `GET /projects/:id/repository/branches/:branch` | <https://docs.gitlab.com/api/branches/index.md> |
| Auth scopes (`api` only — `write_repository` does not authenticate REST API calls) | n/a — policy doc | <https://docs.gitlab.com/user/profile/personal_access_tokens/index.md> |
| Rate limits | n/a — policy doc | <https://docs.gitlab.com/administration/settings/rate_limits/index.md> |
| Error responses | n/a — convention doc | <https://docs.gitlab.com/api/rest/troubleshooting/index.md> |

## How to use

Through the `WebFetch` tool (`docs.gitlab.com` is already allow-listed in `.claude/settings.json`):

```text
WebFetch(
    url="https://docs.gitlab.com/api/commits/index.md",
    prompt="What are the required and optional fields of the POST /projects/:id/repository/commits payload? Specifically the `actions[]` array element shape."
)
```

WebFetch will pull the markdown directly — no HTML noise, no client-side rendering tax.

## When this matters

- **Architect**, before approving a change to `diffActions` or commit-action shape — verify the GitLab payload contract hasn't drifted.
- **Developer**, before adding/changing a field on `CommitActionOptions` — confirm the field name and the accepted values upstream.
- **Security**, when auditing what scope a token needs, what error codes leak which info, and what rate limits apply.
- **Tester**, when writing an acceptance test that exercises a less-common code path (e.g., chmod-only, base64 content) — confirm GitLab actually supports the shape.

## Gotchas

- Some pages redirect to a sign-in flow (e.g. `/api/commits.md` without the `/index.md` suffix returns a 302 to projects.gitlab.io). Always use the `<path>/index.md` form, not `<path>.md`.
- Versioned docs: `docs.gitlab.com` serves the current GA version by default. If a check needs a specific version, use `docs.gitlab.com/<version>/<path>/index.md` (e.g. `/17.4/api/commits/index.md`).
- Self-hosted GitLab instances may run older API versions than docs.gitlab.com. When the provider needs to support an older floor, cross-check against the version-pinned URL.
