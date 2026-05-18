---
name: terraform-framework-patterns
description: Terraform Plugin Framework (NOT SDKv2) patterns used in this provider — schema types, plan modifiers, validators, diagnostics, ID composition, import, and the resource lifecycle. Load when designing or editing any Resource / DataSource / Provider struct in internal/provider/.
---

# Terraform Plugin Framework — patterns we use

This provider is built on **terraform-plugin-framework**, not SDKv2. Behavior differs.

## Schema basics

- Use the `schema` package from `github.com/hashicorp/terraform-plugin-framework/resource/schema` (resources) or `.../datasource/schema` (data sources).
- Every attribute carries `MarkdownDescription` — it's the source of truth for generated docs.
- `Required`, `Optional`, `Computed` are exclusive in the usual way; `Optional + Computed` means "user can set, otherwise we fill in."
- `Sensitive: true` redacts values from plan output and logs (`token` uses this).

## Plan modifiers we rely on

- `stringplanmodifier.RequiresReplace()` — `project_id`, `branch` are tagged with this; any change forces recreate.
- `stringplanmodifier.UseStateForUnknown()` — for computed fields we want to keep stable across plans when not changing.
- Custom plan modifiers go in `internal/provider/` next to the resource using them.

## Defaults

Plugin Framework's built-in defaults (`booldefault.StaticBool`, `stringdefault.StaticString`, etc.) are fine for simple cases. We have a couple of local helpers in `schema_defaults.go` where the built-ins didn't fit.

## Validators

The framework lacks built-in regex and mutual-exclusion validators. Ours live in `schema_validators.go`:

- `regexMatch(pattern, message)` — for path/branch validation.
- `nonEmpty()` — strings must be non-zero.
- `mutuallyExclusive(siblings...)` — content vs content_base64 vs source_file.
- `mapKeyRegex(pattern)` — keys in the `files` map.
- `mapNonEmpty()` — the `files` map can't be empty at update time.

Add validators here rather than scattering them.

## Diagnostics

- Always use `resp.Diagnostics.AddError(summary, detail)` / `AddAttributeError(path, summary, detail)`. Never `panic`.
- Wrap GitLab API failures through `apiErrorDiag` — it inspects the response body to convert concurrent-modification 400s into actionable diagnostics. Do not bypass it.
- A returned diagnostic with `HasError()` short-circuits the CRUD call. Honor this — after every API call, check.

## ID composition

`gitlabcommits_files` ID is `"<project_id>::<branch>"`. The `::` separator was chosen because GitLab project paths can contain `/`. ImportState parses this. Do not change the separator.

## ImportState

```go
func (r *FilesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
    // parses "<project_id>::<branch>" → sets project_id and branch
}
```

After import, the next `apply` runs `Read` which fills `files` from the live branch. The `adopt_existing = true` default makes this idempotent.

## State types vs config types

Use the framework's typed values: `types.String`, `types.Bool`, `types.Int64`, `types.Map`, `types.Object`. For nested objects in a map use `types.MapType{ElemType: types.ObjectType{AttrTypes: ...}}`.

When reading nested maps in CRUD:

```go
var plan FilesResourceModel
diags := req.Plan.Get(ctx, &plan)
resp.Diagnostics.Append(diags...)
if resp.Diagnostics.HasError() { return }
```

Convert `types.Map` of objects to Go maps with `plan.Files.ElementsAs(ctx, &goMap, false)`.

## CRUD return contract

- `Create`: set the full state at the end via `resp.State.Set(ctx, &state)`.
- `Read`: if the resource no longer exists upstream, call `resp.State.RemoveResource(ctx)` and return (no error). Otherwise set full state.
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
- `examples/data-sources/<ds>/data-source.tf` → docs example block.
- `examples/provider/provider.tf` → provider config block.

Edit schemas + examples, then `just docs`. Never hand-edit `docs/`.
