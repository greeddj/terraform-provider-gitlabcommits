---
name: terraform-framework-patterns
description: Terraform Plugin Framework (NOT SDKv2) patterns used in this provider — schema types, plan modifiers, validators, diagnostics, ID composition, import, and the resource lifecycle. Load when designing or editing any Resource / DataSource / Provider struct in internal/provider/.
---

# Terraform Plugin Framework — patterns we use

This provider is built on **terraform-plugin-framework**, not SDKv2. Behavior differs.

## Schema basics

- Use the `schema` package from `github.com/hashicorp/terraform-plugin-framework/resource/schema` (resources) or `.../datasource/schema` (data sources).
- Every attribute carries a `Description` (`MarkdownDescription` also works) - it's the source of truth for generated docs.
- `Required`, `Optional`, `Computed` are exclusive in the usual way; `Optional + Computed` means "user can set, otherwise we fill in."
- `Sensitive: true` redacts values from plan output and logs (`token` uses this).

## Plan modifiers we rely on

- `stringplanmodifier.RequiresReplace()` — `project_id`, `branch` are tagged with this; any change forces recreate.
- `stringplanmodifier.UseStateForUnknown()` — for computed fields we want to keep stable across plans when not changing.
- Custom plan modifiers go in `internal/provider/` next to the resource using them.

## Defaults

Plugin Framework's built-in defaults (`booldefault.StaticBool`, `stringdefault.StaticString`, etc.) cover every default this provider needs; they are declared inline in the schema. There are no local default helpers - do not create a defaults file for a case a built-in already handles.

## Validators

The framework lacks built-in regex and mutual-exclusion validators. Ours live in `schema_validators.go`:

- `stringMatchesRegex(pattern, msg)` - project_id / branch shape validation.
- `stringNotEmpty()` - rejects empty and whitespace-only strings.
- `stringConflictsWithSibling(name)` - content vs content_base64 mutual exclusion (each side names the other).
- `objectFileContentRequired()` - plan-time "at least one of content / content_base64" per file object.
- `mapKeysMatchRegex(pattern, msg)` - keys in the `files` map (repository paths).
- `mapNonEmpty()` - the `files` map can't be empty at update time.

Add validators here rather than scattering them.

## Diagnostics

- Always use `resp.Diagnostics.AddError(summary, detail)` / `AddAttributeError(path, summary, detail)`. Never `panic`.
- Wrap GitLab API failures through `apiErrorDiag` — it inspects the response body to convert concurrent-modification 400s into actionable diagnostics. Do not bypass it.
- A returned diagnostic with `HasError()` short-circuits the CRUD call. Honor this — after every API call, check.

## ID composition

`gitlabcommits_files` ID is `"<project_id>::<branch>"`. The `::` separator was chosen because GitLab project paths can contain `/`. ImportState parses this. Do not change the separator.

## ImportState

```go
func (r *filesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
    // parses "<project_id>::<branch>" -> sets project_id, branch, and id
}
```

After import only `project_id`, `branch`, and `id` are in state - the `files` map is empty, and `Read` never fills it from the branch on its own. Convergence happens on the next plan/apply: the plan treats every HCL file as new, and `adopt_existing = true` (default) rewrites those create actions into updates for paths already present in the repo, forwarding the probed `last_commit_id` under `optimistic_lock`.

## State types vs config types

Use the framework's typed values for scalars: `types.String`, `types.Bool`, `types.Int64`. For the nested `files` map the model uses a plain Go map with `tfsdk` tags (`Files map[string]fileModel`), so `Get` populates it directly - no `types.Map` + `ElementsAs` conversion step exists or is needed:

```go
var plan filesResourceModel
resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
if resp.Diagnostics.HasError() { return }
```

## CRUD return contract

- `Create`: set the full state at the end via `resp.State.Set(ctx, &state)`.
- `Read`: files that vanished upstream are dropped from the `files` map (the next plan recreates them); full state is always written back. The resource itself is never removed from state - `RemoveResource` is not called anywhere in this provider, even when the whole branch is gone.
- `Update`: set full state at the end. If nothing changed (action-diffing yielded zero actions), still write the state back unchanged so computed fields are preserved.
- `Delete`: no state write needed.

## Data sources

- Construct via `provider.DataSources()` — currently `gitlabcommits_file` and `gitlabcommits_branch_head`.
- Same schema/validator helpers apply.
- Data sources don't persist state — `Read` returns the value to the framework.

## Docs generation

`tfplugindocs` reads:
- `MarkdownDescription` from schema → `docs/resources/<name>.md` and `docs/data-sources/<name>.md`.
- `examples/resources/<resource>/resource.tf` → docs example block.
- `examples/data-sources/<ds>/data-source.tf` → docs example block (none exist yet; adding them is what gives the data-source pages an Example Usage section).
- `examples/provider/provider.tf` → provider config block.

Edit schemas + examples, then `just docs`. Never hand-edit `docs/`.
