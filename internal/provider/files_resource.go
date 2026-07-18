// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	gitlab "gitlab.com/gitlab-org/api/client-go"
	"golang.org/x/sync/errgroup"
)

// refreshParallelism caps concurrent GitLab API calls during a single Read.
// 16 is well below GitLab.com's authenticated rate budget while giving a
// 16× speedup on the documented fan-out use case (hundreds of files per
// resource). The retry layer in the client handles 429s if we do hit a cap.
const refreshParallelism = 16

var (
	_ resource.Resource                = &filesResource{}
	_ resource.ResourceWithConfigure   = &filesResource{}
	_ resource.ResourceWithImportState = &filesResource{}
)

func NewFilesResource() resource.Resource {
	return &filesResource{}
}

type filesResource struct {
	client *gitlab.Client
}

type filesResourceModel struct {
	Files            map[string]fileModel `tfsdk:"files"`
	CreateBranchFrom types.String         `tfsdk:"create_branch_from"`
	ProjectID        types.String         `tfsdk:"project_id"`
	Branch           types.String         `tfsdk:"branch"`
	CommitMessage    types.String         `tfsdk:"commit_message"`
	AuthorEmail      types.String         `tfsdk:"author_email"`
	AuthorName       types.String         `tfsdk:"author_name"`
	ID               types.String         `tfsdk:"id"`
	CommitSHA        types.String         `tfsdk:"commit_sha"`
	DetectDrift      types.Bool           `tfsdk:"detect_drift"`
	OptimisticLock   types.Bool           `tfsdk:"optimistic_lock"`
	AdoptExisting    types.Bool           `tfsdk:"adopt_existing"`
	DeleteOnDestroy  types.Bool           `tfsdk:"delete_on_destroy"`
}

type fileModel struct {
	Content         types.String `tfsdk:"content"`
	ContentBase64   types.String `tfsdk:"content_base64"`
	BlobID          types.String `tfsdk:"blob_id"`
	LastCommitID    types.String `tfsdk:"last_commit_id"`
	ExecuteFilemode types.Bool   `tfsdk:"execute_filemode"`
}

func (r *filesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_files"
}

func (r *filesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a set of files in a single GitLab repository on a single branch. " +
			"Every change (create / update / delete / chmod) is batched into ONE commit per terraform apply, " +
			"which means one CI pipeline run per resource. Use one resource per logical bundle " +
			"(typically per service) so each apply produces exactly one commit per service.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "Composite identifier: \"<project_id>::<branch>\".",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": schema.StringAttribute{
				Description: "Project ID or URL-encoded path (e.g. \"group/project\" or \"12345\").",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringNotEmpty(),
					// Either a numeric ID, or a slash-delimited path of segments. Lenient on
					// allowed characters because GitLab accepts mixed-case + dots + dashes.
					stringMatchesRegex(
						`^([0-9]+|[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*)$`,
						"must be a numeric ID or a slash-separated project path like \"group/subgroup/project\"",
					),
				},
			},
			"branch": schema.StringAttribute{
				Description: "Target branch. Must already exist, or set create_branch_from to materialise it.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringNotEmpty(),
					// Permissive character allowlist only: letters, digits, dot,
					// underscore, dash, slash. Does NOT reject "..", a leading or
					// trailing slash, or other git-invalid shapes; GitLab validates
					// those server-side. This only blocks whitespace and exotic chars.
					stringMatchesRegex(
						`^[A-Za-z0-9_./-]+$`,
						"branch name can only contain letters, digits, dot, underscore, dash, and slash",
					),
				},
			},
			"commit_message": schema.StringAttribute{
				Description: "Message used for any commit (create / update / destroy) the resource produces. " +
					"Only takes effect on an apply that actually changes file content, mode, or set; editing just " +
					"commit_message or the author fields produces no commit, so the new value applies to the next change.",
				Required: true,
				Validators: []validator.String{
					stringNotEmpty(),
				},
			},
			"author_email": schema.StringAttribute{
				Description: "Optional override for commit author email.",
				Optional:    true,
			},
			"author_name": schema.StringAttribute{
				Description: "Optional override for commit author name.",
				Optional:    true,
			},
			"detect_drift": schema.BoolAttribute{
				Description: "If true (default), Read fetches each managed file from GitLab and updates state " +
					"when the remote blob differs, so terraform plan reflects the real repository state.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"delete_on_destroy": schema.BoolAttribute{
				Description: "If true (default), terraform destroy creates one commit that removes every managed file. " +
					"Set to false to keep files in place when the resource is removed from state.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"adopt_existing": schema.BoolAttribute{
				Description: "If true (default), files that exist in the repository but are not yet in state are " +
					"adopted on the next apply: a create-action targeting an existing path is silently rewritten " +
					"as an update. When optimistic_lock is enabled, that adopt-update carries the file's current " +
					"commit, so a concurrent external modification is still detected instead of being overwritten. " +
					"Required for terraform import to converge cleanly.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"create_branch_from": schema.StringAttribute{
				Description: "If set and `branch` does not yet exist, the provider creates it from this " +
					"source ref (typically \"main\") on first apply. Only consulted by Create; once " +
					"the branch exists, changing or removing this value is a state-only no-op " +
					"(no destroy / recreate).",
				Optional: true,
			},
			"optimistic_lock": schema.BoolAttribute{
				Description: "If true (default), update / delete / chmod actions send the file's last_commit_id to GitLab. " +
					"GitLab rejects the action with HTTP 400 if the file has been modified by anyone else since " +
					"this resource last touched it, preventing silent overwrites in concurrent pipelines. " +
					"Set to false to opt out (useful when an external process intentionally co-edits the same files).",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"commit_sha": schema.StringAttribute{
				Description: "SHA of the last commit produced by this resource.",
				Computed:    true,
			},
			"files": schema.MapNestedAttribute{
				Description: "Map of repository_path → file definition. The map key is the path inside the repo.",
				Required:    true,
				Validators: []validator.Map{
					mapNonEmpty(),
					// Repository paths: no leading slash, no `..` or `.` segments, no NUL
					// bytes, no leading whitespace per segment. Interior spaces and other
					// characters are tolerated - git accepts quite a lot.
					mapKeysMatchRegex(
						`^(?:[^/\s\x00.][^/\x00]*|\.[^./\x00][^/\x00]*)(?:/(?:[^/\s\x00.][^/\x00]*|\.[^./\x00][^/\x00]*))*$`,
						"file paths must be relative (no leading slash), must not contain `..` or NUL bytes",
					),
				},
				NestedObject: schema.NestedAttributeObject{
					Validators: []validator.Object{
						objectFileContentRequired(),
					},
					Attributes: map[string]schema.Attribute{
						"content": schema.StringAttribute{
							Description: "Text content. Mutually exclusive with content_base64. Not intended for secret " +
								"values: it is stored in plaintext in state and printed in plan / apply output (and thus CI logs). " +
								"Deliver secrets via SealedSecrets / ExternalSecrets / Vault and reference them from the managed file.",
							Optional: true,
							Validators: []validator.String{
								stringConflictsWithSibling("content_base64"),
							},
						},
						"content_base64": schema.StringAttribute{
							Description: "Base64-encoded content (use for binaries). Mutually exclusive with content. " +
								"Not intended for secret values (see content).",
							Optional: true,
							Validators: []validator.String{
								stringConflictsWithSibling("content"),
							},
						},
						"execute_filemode": schema.BoolAttribute{
							Description: "Whether the file should have the executable bit set.",
							Optional:    true,
							Computed:    true,
							Default:     booldefault.StaticBool(false),
						},
						"blob_id": schema.StringAttribute{
							Description: "Opaque blob identifier returned by GitLab; used for drift detection. " +
								"Format is GitLab-specific (git SHA-1 today, possibly SHA-256 on SHA-256 repositories).",
							Computed: true,
						},
						"last_commit_id": schema.StringAttribute{
							Description: "SHA of the last commit through which this resource touched the file. " +
								"When optimistic_lock is enabled, sent to GitLab on update / delete to detect " +
								"concurrent modifications.",
							Computed: true,
						},
					},
				},
			},
		},
	}
}

func (r *filesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*gitlab.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *gitlab.Client, got: %T. Please report this to the provider developers.", req.ProviderData),
		)
		return
	}
	r.client = client
}

// Create pushes one commit that materialises every file in the plan. If
// adopt_existing is true (default), pre-existing paths are rewritten from
// "create" to "update" so we don't fail with "file already exists".
func (r *filesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan filesResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if len(plan.Files) == 0 {
		resp.Diagnostics.AddAttributeError(path.Root("files"),
			"Empty files map",
			"At least one file must be defined.")
		return
	}

	if err := r.ensureBranch(ctx,
		plan.ProjectID.ValueString(),
		plan.Branch.ValueString(),
		plan.CreateBranchFrom.ValueString(),
	); err != nil {
		summary, detail := apiErrorDiag("ensuring branch exists", plan.ProjectID.ValueString(), plan.Branch.ValueString(), err)
		if summary == "" {
			summary = "Branch unavailable"
			detail = err.Error()
		}
		resp.Diagnostics.AddError(summary, detail)
		return
	}

	paths := sortedKeys(plan.Files)
	useLock := plan.optimisticLock()
	var probes map[string]remoteProbe
	if plan.adoptExisting() {
		probes = r.probeRemote(ctx, plan.ProjectID.ValueString(), plan.Branch.ValueString(), paths)
	}

	actions := make([]*gitlab.CommitActionOptions, 0, len(plan.Files))
	for _, p := range paths {
		acts, err := adoptAwareActions(p, plan.Files[p], probes[p], useLock)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("files").AtMapKey(p), "Invalid file", err.Error())
			return
		}
		actions = append(actions, acts...)
	}

	tflog.Debug(ctx, "Creating GitLab files commit", map[string]any{
		"project_id": plan.ProjectID.ValueString(),
		"branch":     plan.Branch.ValueString(),
		"actions":    len(actions),
	})

	commit, _, err := r.client.Commits.CreateCommit(
		plan.ProjectID.ValueString(),
		commitOptions(plan, actions),
		gitlab.WithContext(ctx),
	)
	if err != nil {
		summary, detail := apiErrorDiag("creating commit", plan.ProjectID.ValueString(), plan.Branch.ValueString(), err)
		resp.Diagnostics.AddError(summary, detail)
		return
	}
	// A 2xx response with a JSON-null body decodes to a nil *Commit with no
	// error; guard before dereferencing so a hostile/buggy GitLab cannot panic
	// the provider process after a commit may already have landed.
	if commit == nil {
		resp.Diagnostics.AddError("GitLab returned no commit",
			"CreateCommit succeeded but the response contained no commit object; repository state is unknown. Run `terraform plan` to reconcile.")
		return
	}

	resp.Diagnostics.Append(r.stampBlobs(ctx, plan.ProjectID.ValueString(), plan.Branch.ValueString(), plan.Files, commit.ID)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.CommitSHA = types.StringValue(commit.ID)
	plan.ID = types.StringValue(buildID(plan.ProjectID.ValueString(), plan.Branch.ValueString()))

	tflog.Info(ctx, "GitLab files commit created", map[string]any{
		"project_id": plan.ProjectID.ValueString(),
		"branch":     plan.Branch.ValueString(),
		"commit_sha": commit.ID,
		"files":      len(actions),
	})

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// Read refreshes blob_id (and content) for every managed file, dropping files
// that no longer exist in the repository so the next plan recreates them.
// If detect_drift is false the resource is treated as opaque after creation.
//
// Two-step probe: GetFileMetaData first (HEAD-style, no body) to compare
// blob_id; only fetch the full content via GetFile when the blob has
// drifted. For the typical fan-out use case (hundreds of unchanged files
// per resource) this turns a refresh from N full downloads into N metadata
// calls plus zero-or-few content downloads, fanned out at refreshParallelism.
//
// The probe runs concurrently; state mutation is deferred to a serial second
// pass so we never write to state.Files from multiple goroutines.
func (r *filesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state filesResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !state.detectDrift() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}

	branch := state.Branch.ValueString()
	project := state.ProjectID.ValueString()

	paths := sortedKeys(state.Files)
	results := make([]fileRefreshResult, len(paths))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(refreshParallelism)
	for i, p := range paths {
		f := state.Files[p]
		g.Go(func() error {
			meta, _, err := r.client.RepositoryFiles.GetFileMetaData(project, p, &gitlab.GetFileMetaDataOptions{
				Ref: new(branch),
			}, gitlab.WithContext(gctx))
			if err != nil {
				if errors.Is(err, gitlab.ErrNotFound) {
					results[i].drop = true
					return nil
				}
				return &pathError{path: p, err: err}
			}
			if meta.BlobID == f.BlobID.ValueString() &&
				meta.ExecuteFilemode == f.ExecuteFilemode.ValueBool() {
				results[i].metaLastCommitID = meta.LastCommitID
				return nil
			}
			file, _, err := r.client.RepositoryFiles.GetFile(project, p, &gitlab.GetFileOptions{
				Ref: new(branch),
			}, gitlab.WithContext(gctx))
			if err != nil {
				return &pathError{path: p, err: err}
			}
			// A nil *File (2xx JSON-null body) for a blob we already know drifted
			// must not fall through and be treated as "unchanged" below.
			if file == nil {
				return &pathError{path: p, err: errors.New("GitLab returned an empty file object")}
			}
			results[i].file = file
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		var pe *pathError
		if errors.As(err, &pe) {
			summary, detail := apiErrorDiag(fmt.Sprintf("reading file %q", pe.path), project, branch, pe.err)
			resp.Diagnostics.AddError(summary, detail)
			return
		}
		resp.Diagnostics.AddError("Refresh failed", err.Error())
		return
	}

	for i, p := range paths {
		res := results[i]
		if res.drop {
			tflog.Info(ctx, "managed file is gone, dropping from state",
				map[string]any{"path": p, "branch": branch})
			delete(state.Files, p)
			continue
		}
		f := state.Files[p]
		if res.file == nil {
			// Took the metadata-only branch (blob unchanged) — refresh
			// last_commit_id only when it has moved (a delete-then-re-add with
			// identical content would otherwise stale the optimistic-lock token
			// in state). A nil *File from a drifted blob is surfaced as an error
			// in the probe above, so it never reaches here.
			if res.metaLastCommitID != "" && res.metaLastCommitID != f.LastCommitID.ValueString() {
				f.LastCommitID = types.StringValue(res.metaLastCommitID)
				state.Files[p] = f
			}
			continue
		}
		raw, err := decodeRemoteContent(res.file)
		if err != nil {
			resp.Diagnostics.AddError("Cannot decode remote file content",
				fmt.Sprintf("file %q: %s", p, err))
			return
		}
		if len(res.file.BlobID) > maxBlobIDLen {
			// Treat an absurdly long blob_id as hostile: leave it unset rather
			// than persisting it. A server that keeps returning an oversized
			// blob_id makes every Read re-fetch content (the null we store never
			// equals the oversized HEAD blob, so drift never settles), but Read
			// never commits or persists a wrong value, so the only cost is
			// repeated GETs against a misbehaving server.
			resp.Diagnostics.AddWarning("Ignoring oversized blob_id",
				fmt.Sprintf("file %q: server returned blob_id of unexpected length %d (max %d); leaving blob_id unset", p, len(res.file.BlobID), maxBlobIDLen))
			f.BlobID = types.StringNull()
		} else {
			f.BlobID = types.StringValue(res.file.BlobID)
		}
		f.ExecuteFilemode = types.BoolValue(res.file.ExecuteFilemode)
		if res.file.LastCommitID != "" {
			f.LastCommitID = types.StringValue(res.file.LastCommitID)
		}
		// Preserve whichever form the user originally chose.
		if !f.ContentBase64.IsNull() {
			f.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString(raw))
		} else {
			// cty silently replaces invalid UTF-8 with U+FFFD, so storing
			// binary drift into the text attribute would corrupt state and
			// produce a diff that can never converge.
			if !utf8.Valid(raw) {
				resp.Diagnostics.AddError("Remote file is not valid UTF-8",
					fmt.Sprintf("file %q drifted to binary content that cannot be represented in the text `content` attribute; manage this file via `content_base64` instead", p))
				return
			}
			f.Content = types.StringValue(string(raw))
		}
		state.Files[p] = f
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// fileRefreshResult is the per-file outcome of a parallel refresh probe.
// Exactly one of (drop, metaLastCommitID-only, file) holds the answer;
// the rest are zero values.
type fileRefreshResult struct {
	file             *gitlab.File // non-nil iff blob drifted and content was pulled
	metaLastCommitID string       // non-empty iff blob unchanged
	drop             bool         // file was deleted at the remote
}

// pathError attaches a repository path to an underlying error so the parallel
// refresh can surface "which file failed" through errgroup.Group.Wait without
// dropping the original *gitlab.ErrorResponse needed by apiErrorDiag.
type pathError struct {
	err  error
	path string
}

func (e *pathError) Error() string { return fmt.Sprintf("file %q: %v", e.path, e.err) }
func (e *pathError) Unwrap() error { return e.err }

// Update reconciles plan vs state by emitting only the actions that are
// actually needed (create / update / delete / chmod) and pushing them as one
// commit. If nothing changed, no commit is produced.
func (r *filesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state filesResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	actions, err := r.diffActions(ctx, plan, state)
	if err != nil {
		resp.Diagnostics.AddError("Error building actions", err.Error())
		return
	}

	if len(actions) == 0 {
		// No file content changed; just preserve computed fields.
		for p, f := range plan.Files {
			if existing, ok := state.Files[p]; ok {
				f.BlobID = existing.BlobID
				f.LastCommitID = existing.LastCommitID
			}
			plan.Files[p] = f
		}
		plan.CommitSHA = state.CommitSHA
		plan.ID = state.ID
		resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
		return
	}

	tflog.Debug(ctx, "Updating GitLab files commit", map[string]any{
		"project_id": plan.ProjectID.ValueString(),
		"branch":     plan.Branch.ValueString(),
		"actions":    len(actions),
	})

	commit, _, err := r.client.Commits.CreateCommit(
		plan.ProjectID.ValueString(),
		commitOptions(plan, actions),
		gitlab.WithContext(ctx),
	)
	if err != nil {
		summary, detail := apiErrorDiag("pushing update commit", plan.ProjectID.ValueString(), plan.Branch.ValueString(), err)
		resp.Diagnostics.AddError(summary, detail)
		return
	}
	// See Create: a JSON-null body decodes to a nil *Commit with no error.
	if commit == nil {
		resp.Diagnostics.AddError("GitLab returned no commit",
			"CreateCommit succeeded but the response contained no commit object; repository state is unknown. Run `terraform plan` to reconcile.")
		return
	}

	resp.Diagnostics.Append(r.stampBlobs(ctx, plan.ProjectID.ValueString(), plan.Branch.ValueString(), plan.Files, commit.ID)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.CommitSHA = types.StringValue(commit.ID)
	plan.ID = types.StringValue(buildID(plan.ProjectID.ValueString(), plan.Branch.ValueString()))

	tflog.Info(ctx, "GitLab files commit pushed", map[string]any{
		"project_id": plan.ProjectID.ValueString(),
		"branch":     plan.Branch.ValueString(),
		"commit_sha": commit.ID,
		"actions":    len(actions),
	})

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// Delete pushes one commit that removes every managed file. Files already
// missing on the remote are silently skipped so destroy is idempotent against
// out-of-band cleanup. Disabled by setting delete_on_destroy = false.
func (r *filesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state filesResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !state.deleteOnDestroy() {
		return
	}

	project := state.ProjectID.ValueString()
	branch := state.Branch.ValueString()
	useLock := state.optimisticLock()

	paths := sortedKeys(state.Files)
	probes := r.probeRemote(ctx, project, branch, paths)

	// A failed probe must fail the destroy: treating it as "absent" would skip
	// the delete action and let the framework drop the resource from state
	// while the file may still exist in the repository, silently orphaned.
	for _, p := range paths {
		if err := probes[p].err; err != nil {
			summary, detail := apiErrorDiag(fmt.Sprintf("probing file %q before destroy", p), project, branch, err)
			resp.Diagnostics.AddError(summary, detail)
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	actions := make([]*gitlab.CommitActionOptions, 0, len(state.Files))
	for _, p := range paths {
		if !probes[p].exists {
			continue
		}
		a := &gitlab.CommitActionOptions{
			Action:   gitlab.Ptr(gitlab.FileDelete),
			FilePath: new(p),
		}
		if useLock {
			if lcid := state.Files[p].LastCommitID.ValueString(); lcid != "" {
				a.LastCommitID = new(lcid)
			}
		}
		actions = append(actions, a)
	}

	if len(actions) == 0 {
		return
	}

	_, _, err := r.client.Commits.CreateCommit(project, commitOptions(state, actions), gitlab.WithContext(ctx))
	if err != nil {
		summary, detail := apiErrorDiag("pushing destroy commit", project, branch, err)
		resp.Diagnostics.AddError(summary, detail)
	}
}

// ImportState supports importing by "<project_id>::<branch>". After import the
// files map is empty; running terraform plan will then reconcile the user's
// HCL with the repo, and adopt_existing=true keeps it from blowing up on
// already-present paths.
func (r *filesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	project, branch, err := parseImportID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid Import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), types.StringValue(project))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("branch"), types.StringValue(branch))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), types.StringValue(buildID(project, branch)))...)
}

// parseImportID splits a composite import identifier of the form
// "<project_id>::<branch>" into its two components. Both parts must be
// non-empty and non-whitespace; multiple "::" separators (e.g. "a::b::c")
// are rejected so the caller never silently keeps part of the suffix.
func parseImportID(s string) (project, branch string, err error) {
	parts := strings.Split(s, "::")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("expected \"project_id::branch\", got %q", s)
	}
	return parts[0], parts[1], nil
}

// diffActions computes the minimal set of commit actions needed to make the
// repository match the plan. For files newly added in the plan, when
// adopt_existing is enabled, it prefers update over create if the path already
// exists in the repo (e.g. after terraform import). When optimistic_lock is
// enabled, update / delete / chmod actions carry the file's previously-known
// last_commit_id so GitLab rejects the action if the file was concurrently
// modified.
func (r *filesResource) diffActions(ctx context.Context, plan, state filesResourceModel) ([]*gitlab.CommitActionOptions, error) {
	actions := make([]*gitlab.CommitActionOptions, 0)
	useLock := plan.optimisticLock()

	// Probe new-in-plan paths in parallel when adoption is on. The
	// post-import path (state.Files empty, plan.Files large) hits this with
	// every managed file marked "new", so a sequential probe per path
	// would dominate the apply latency.
	var probes map[string]remoteProbe
	if plan.adoptExisting() {
		var newPaths []string
		for _, p := range sortedKeys(plan.Files) {
			if _, ok := state.Files[p]; !ok {
				newPaths = append(newPaths, p)
			}
		}
		if len(newPaths) > 0 {
			probes = r.probeRemote(ctx, plan.ProjectID.ValueString(), plan.Branch.ValueString(), newPaths)
		}
	}

	for _, p := range sortedKeys(plan.Files) {
		pf := plan.Files[p]
		sf, exists := state.Files[p]

		if !exists {
			acts, err := adoptAwareActions(p, pf, probes[p], useLock)
			if err != nil {
				return nil, fmt.Errorf("file %q: %w", p, err)
			}
			actions = append(actions, acts...)
			continue
		}

		raw, err := pf.rawBytes()
		if err != nil {
			return nil, fmt.Errorf("file %q: %w", p, err)
		}

		lastCommitID := ""
		if useLock {
			lastCommitID = sf.LastCommitID.ValueString()
		}

		stateRaw, err := sf.rawBytes()
		if err != nil {
			return nil, fmt.Errorf("file %q: %w", p, err)
		}
		if !bytes.Equal(raw, stateRaw) {
			a, err := buildAction(p, pf, gitlab.FileUpdate, lastCommitID)
			if err != nil {
				return nil, fmt.Errorf("file %q: %w", p, err)
			}
			actions = append(actions, a)
		}

		planExec := pf.ExecuteFilemode.ValueBool()
		stateExec := sf.ExecuteFilemode.ValueBool()
		if planExec != stateExec {
			chmod := &gitlab.CommitActionOptions{
				Action:          gitlab.Ptr(gitlab.FileChmod),
				FilePath:        new(p),
				ExecuteFilemode: new(planExec),
			}
			if lastCommitID != "" {
				chmod.LastCommitID = new(lastCommitID)
			}
			actions = append(actions, chmod)
		}
	}

	for _, p := range sortedKeys(state.Files) {
		if _, kept := plan.Files[p]; kept {
			continue
		}
		del := &gitlab.CommitActionOptions{
			Action:   gitlab.Ptr(gitlab.FileDelete),
			FilePath: new(p),
		}
		if useLock {
			if lcid := state.Files[p].LastCommitID.ValueString(); lcid != "" {
				del.LastCommitID = new(lcid)
			}
		}
		actions = append(actions, del)
	}

	return actions, nil
}

// remoteProbe is the result of a HEAD-style metadata probe for one path:
// whether the file exists at branch HEAD and, if so, its current
// last_commit_id. The token lets an adopt-update be guarded by optimistic_lock
// even though there is no prior state for the file. err is set for any probe
// failure other than a genuine 404 so callers can tell "absent" from
// "unknown": the adopt paths deliberately ignore it (a spurious create fails
// loudly at CreateCommit anyway), but Delete must not - skipping a file
// because a probe errored would let destroy report success while the file
// still exists in the repository.
type remoteProbe struct {
	err             error
	lastCommitID    string
	exists          bool
	executeFilemode bool
}

// probeFile reports whether filePath is present at branch HEAD and returns its
// last_commit_id. Used to rewrite spurious "create" actions into "update" when
// adopting existing files, to forward a lock token on that adopt-update, and to
// skip already-deleted paths during destroy. Only a genuine 404 maps to
// "absent"; any other failure is carried in err.
func (r *filesResource) probeFile(ctx context.Context, project, branch, filePath string) remoteProbe {
	meta, _, err := r.client.RepositoryFiles.GetFileMetaData(project, filePath, &gitlab.GetFileMetaDataOptions{
		Ref: new(branch),
	}, gitlab.WithContext(ctx))
	if err != nil {
		if errors.Is(err, gitlab.ErrNotFound) {
			return remoteProbe{}
		}
		return remoteProbe{err: err}
	}
	if meta == nil {
		return remoteProbe{}
	}
	return remoteProbe{exists: true, lastCommitID: meta.LastCommitID, executeFilemode: meta.ExecuteFilemode}
}

// adoptAwareActions builds the action set for a path with no prior state: a
// plain create, or - when the path already exists remotely - an adopt-update
// carrying the probed lock token. The commits API honors execute_filemode only
// on create and chmod actions, so an adopt-update cannot set the exec bit
// itself; when the remote bit differs from the plan a companion chmod is
// emitted into the same commit, keeping one-commit-per-apply intact.
func adoptAwareActions(p string, f fileModel, probe remoteProbe, useLock bool) ([]*gitlab.CommitActionOptions, error) {
	op := gitlab.FileCreate
	lastCommitID := ""
	if probe.exists {
		op = gitlab.FileUpdate
		// Forward the probed last_commit_id under optimistic_lock so the
		// adopt-update still fails on a concurrent writer instead of blindly
		// overwriting it.
		if useLock {
			lastCommitID = probe.lastCommitID
		}
	}
	a, err := buildAction(p, f, op, lastCommitID)
	if err != nil {
		return nil, err
	}
	actions := []*gitlab.CommitActionOptions{a}
	if probe.exists && probe.executeFilemode != f.ExecuteFilemode.ValueBool() {
		chmod := &gitlab.CommitActionOptions{
			Action:          gitlab.Ptr(gitlab.FileChmod),
			FilePath:        new(p),
			ExecuteFilemode: new(f.ExecuteFilemode.ValueBool()),
		}
		if lastCommitID != "" {
			chmod.LastCommitID = new(lastCommitID)
		}
		actions = append(actions, chmod)
	}
	return actions, nil
}

// probeRemote fans probeFile out across paths at refreshParallelism. Errors are
// swallowed because probeFile itself swallows errors (a transport hiccup is
// treated as "absent", same as the sequential code).
func (r *filesResource) probeRemote(ctx context.Context, project, branch string, paths []string) map[string]remoteProbe {
	out := make(map[string]remoteProbe, len(paths))
	if len(paths) == 0 {
		return out
	}
	probes := make([]remoteProbe, len(paths))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(refreshParallelism)
	for i, p := range paths {
		g.Go(func() error {
			probes[i] = r.probeFile(gctx, project, branch, p)
			return nil
		})
	}
	_ = g.Wait()
	for i, p := range paths {
		out[p] = probes[i]
	}
	return out
}

// ensureBranch makes sure the target branch exists, creating it from createFrom
// if it doesn't and createFrom is non-empty. Returns nil when the branch is
// already present, or once it has been successfully created.
func (r *filesResource) ensureBranch(ctx context.Context, project, branch, createFrom string) error {
	_, _, err := r.client.Branches.GetBranch(project, branch, gitlab.WithContext(ctx))
	if err == nil {
		return nil
	}
	if !errors.Is(err, gitlab.ErrNotFound) {
		return fmt.Errorf("checking branch %q: %w", branch, err)
	}
	if createFrom == "" {
		return fmt.Errorf("branch %q does not exist; set create_branch_from to materialise it", branch)
	}
	_, _, err = r.client.Branches.CreateBranch(project, &gitlab.CreateBranchOptions{
		Branch: new(branch),
		Ref:    new(createFrom),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("creating branch %q from %q: %w", branch, createFrom, err)
	}
	tflog.Info(ctx, "Created GitLab branch", map[string]any{
		"project_id": project,
		"branch":     branch,
		"from":       createFrom,
	})
	return nil
}

// --- helpers ---

func (m filesResourceModel) detectDrift() bool {
	if m.DetectDrift.IsNull() || m.DetectDrift.IsUnknown() {
		return true
	}
	return m.DetectDrift.ValueBool()
}

func (m filesResourceModel) deleteOnDestroy() bool {
	if m.DeleteOnDestroy.IsNull() || m.DeleteOnDestroy.IsUnknown() {
		return true
	}
	return m.DeleteOnDestroy.ValueBool()
}

func (m filesResourceModel) adoptExisting() bool {
	if m.AdoptExisting.IsNull() || m.AdoptExisting.IsUnknown() {
		return true
	}
	return m.AdoptExisting.ValueBool()
}

func (m filesResourceModel) optimisticLock() bool {
	if m.OptimisticLock.IsNull() || m.OptimisticLock.IsUnknown() {
		return true
	}
	return m.OptimisticLock.ValueBool()
}

// rawBytes returns the file's raw content regardless of whether the user
// supplied content (text) or content_base64.
func (f fileModel) rawBytes() ([]byte, error) {
	switch {
	case !f.Content.IsNull() && !f.Content.IsUnknown():
		return []byte(f.Content.ValueString()), nil
	case !f.ContentBase64.IsNull() && !f.ContentBase64.IsUnknown():
		return base64.StdEncoding.DecodeString(f.ContentBase64.ValueString())
	default:
		return nil, errors.New("either content or content_base64 must be set")
	}
}

// stampBlobs refreshes BlobID and LastCommitID for every entry in files
// after a successful CreateCommit. BlobID is pulled via parallel
// GetFileMetaData probes (HEAD-style) so state reflects what GitLab
// returns — including any future blob-id format (SHA-256 repos).
//
// Probes run at Ref = commitSHA (the commit just created), NOT at branch
// HEAD: a writer landing between our CreateCommit and the probe would
// otherwise have their blob_id and last_commit_id stamped into state next
// to OUR content, permanently blinding drift detection (Read sees the
// blob match) and handing the next locked apply a token that matches the
// racer's commit. Probing our own commit keeps state self-consistent; the
// racer is then caught by the next Read (their blob differs) and by the
// optimistic lock (our commit id no longer matches the file's last
// commit). Per-file LastCommitID still comes from the probe response, not
// commitSHA verbatim: files this commit did not touch keep their older
// commit id, avoiding false lock conflicts.
//
// Fail-soft covers two shapes: probe error AND a server-returned blob_id
// longer than 256 bytes (generous ceiling above SHA-512 hex; anything
// longer is unexpected and treated as hostile). In both cases BlobID is
// left null, LastCommitID keeps the commitSHA first-pass stamp, and a
// warning is appended; the next Read will repopulate both.
func (r *filesResource) stampBlobs(
	ctx context.Context,
	project, branch string,
	files map[string]fileModel,
	commitSHA string,
) diag.Diagnostics {
	var diagnostics diag.Diagnostics

	// First pass (serial): stamp LastCommitID from the known commit SHA and
	// reset BlobID to null so a probe failure leaves it null rather than stale.
	for p, f := range files {
		if commitSHA != "" {
			f.LastCommitID = types.StringValue(commitSHA)
		}
		f.BlobID = types.StringNull()
		files[p] = f
	}

	ref := commitSHA
	if ref == "" {
		ref = branch
	}

	paths := sortedKeys(files)
	type probeResult struct {
		err          error
		blobID       string
		lastCommitID string
	}
	results := make([]probeResult, len(paths))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(refreshParallelism)
	for i, p := range paths {
		g.Go(func() error {
			meta, _, err := r.client.RepositoryFiles.GetFileMetaData(project, p, &gitlab.GetFileMetaDataOptions{
				Ref: new(ref),
			}, gitlab.WithContext(gctx))
			if err != nil {
				results[i].err = err
				return nil //nolint:nilerr // intentional: store per-file error, don't cancel other probes
			}
			if len(meta.BlobID) > maxBlobIDLen {
				results[i].err = fmt.Errorf("path %q: server returned blob_id of unexpected length %d (max %d)", p, len(meta.BlobID), maxBlobIDLen)
				return nil //nolint:nilerr // intentional: treat oversized blob_id as probe failure
			}
			results[i].blobID = meta.BlobID
			results[i].lastCommitID = meta.LastCommitID
			return nil
		})
	}
	_ = g.Wait()

	// Second pass (serial): apply probe results. On success, overwrite the
	// first-pass commitSHA with the server's LastCommitID so a concurrent
	// writer between our commit and this probe is reflected in state.
	for i, p := range paths {
		f := files[p]
		if results[i].err != nil {
			diagnostics = append(diagnostics, diag.NewWarningDiagnostic(
				"Could not refresh blob_id after commit",
				fmt.Sprintf("path %q: %s", p, results[i].err),
			))
		} else {
			f.BlobID = types.StringValue(results[i].blobID)
			f.LastCommitID = types.StringValue(results[i].lastCommitID)
		}
		files[p] = f
	}

	return diagnostics
}

// buildAction translates one fileModel into a CommitActionOptions for the
// given operation, encoding content correctly and applying execute_filemode
// only when relevant. When lastCommitID is non-empty (optimistic_lock + an
// existing file), it is forwarded to GitLab so the API rejects the action
// if the file changed underneath us.
func buildAction(filePath string, f fileModel, op gitlab.FileActionValue, lastCommitID string) (*gitlab.CommitActionOptions, error) {
	a := &gitlab.CommitActionOptions{
		Action:   new(op),
		FilePath: new(filePath),
	}
	switch {
	case !f.Content.IsNull() && !f.Content.IsUnknown():
		a.Content = new(f.Content.ValueString())
		a.Encoding = new("text")
	case !f.ContentBase64.IsNull() && !f.ContentBase64.IsUnknown():
		if _, err := base64.StdEncoding.DecodeString(f.ContentBase64.ValueString()); err != nil {
			return nil, fmt.Errorf("invalid base64: %w", err)
		}
		a.Content = new(f.ContentBase64.ValueString())
		a.Encoding = new("base64")
	default:
		return nil, errors.New("either content or content_base64 must be set")
	}
	// The commits API honors execute_filemode only on create and chmod
	// actions; on update it is silently ignored, so sending it there would
	// just mislead a reader into thinking it takes effect.
	if op == gitlab.FileCreate && f.ExecuteFilemode.ValueBool() {
		a.ExecuteFilemode = new(true)
	}
	if lastCommitID != "" {
		a.LastCommitID = new(lastCommitID)
	}
	return a, nil
}

// maxBlobIDLen bounds a server-returned blob_id we will store in state. GitLab's
// blob_id is a git SHA (40 hex today, 64 on SHA-256 repos); anything past this
// generous ceiling (well above SHA-512 hex) is unexpected and treated as hostile
// so a malicious response cannot bloat Terraform state.
const maxBlobIDLen = 256

// maxDiagBodyChars caps the size of any GitLab response body we splice into
// a Terraform diagnostic. Without this a pathological GitLab error (or a
// reverse-proxy returning a full HTML page) would dump kilobytes into every
// terraform plan / apply output and the local Terraform log.
const maxDiagBodyChars = 1024

func truncateForDiag(s string) string {
	if len(s) <= maxDiagBodyChars {
		return s
	}
	return s[:maxDiagBodyChars] + fmt.Sprintf("… (truncated, %d more chars)", len(s)-maxDiagBodyChars)
}

// apiErrorDiag turns a raw GitLab API error into a structured Terraform
// diagnostic with HTTP status, response body, and the relevant project / branch
// context. Recognises common cases (401/403 token issues, 404 missing
// resource, 409 / 400 optimistic-lock conflicts, 429 rate limiting) and gives
// the user actionable guidance instead of a bare error string.
//
// Callers must guard on err != nil; this function does not.
func apiErrorDiag(action, project, branch string, err error) (string, string) {
	summary := fmt.Sprintf("GitLab API error: %s", action)
	prefix := fmt.Sprintf("project=%q branch=%q", project, branch)

	var resp *gitlab.ErrorResponse
	if errors.As(err, &resp) && resp.Response != nil {
		status := resp.Response.StatusCode
		body := truncateForDiag(resp.Message)
		switch status {
		case 401:
			summary = "GitLab authentication failed (HTTP 401)"
			return summary, fmt.Sprintf("%s: token rejected. Verify the token has the `api` scope and is not expired. Body: %s", prefix, body)
		case 403:
			summary = "GitLab permission denied (HTTP 403)"
			return summary, fmt.Sprintf("%s: %s Body: %s", prefix,
				"The token was rejected by GitLab. Verify that: "+
					"(1) the token has the `api` scope (write_repository alone does not authenticate REST API calls); "+
					"(2) the token's user has the Developer role on the project, or Maintainer for a protected branch; "+
					"(3) if you are using CI_JOB_TOKEN, switch to a Personal / Project / Group access token — job tokens cannot POST to /repository/commits.",
				body)
		case 404:
			summary = "GitLab resource not found (HTTP 404)"
			return summary, fmt.Sprintf("%s: project or branch does not exist. Body: %s", prefix, body)
		case 400, 409:
			// 400 with optimistic-lock failure: GitLab 18 returns
			//   "You are attempting to update a file that has changed since you
			//    started editing it. Try again. File last commit id: <sha>"
			// We match three substrings — `last_commit_id` (snake_case, future-proof if
			// the API ever exposes the parameter name), `last commit` (current prose
			// form), and `has changed since` (most stable phrase) — so any one
			// surviving a future rewording keeps the diagnostic accurate.
			lower := strings.ToLower(resp.Message)
			if strings.Contains(lower, "last_commit_id") ||
				strings.Contains(lower, "last commit") ||
				strings.Contains(lower, "has changed since") {
				summary = "Concurrent modification detected (optimistic_lock)"
				return summary, fmt.Sprintf("%s: a file was modified by someone else since this resource last touched it. "+
					"Run `terraform apply -refresh-only` to pull current state, then re-plan. Body: %s", prefix, body)
			}
			return summary, fmt.Sprintf("%s: HTTP %d. Body: %s", prefix, status, body)
		case 413:
			// GitLab rejects a commit request whose body exceeds a cap (default
			// 300 MB / 314572800 bytes) with 413. one-commit-per-apply batches
			// every file into one request, so we are more prone to this than a
			// per-file client; point the user at splitting the resource, not the
			// commit.
			summary = "GitLab commit too large (HTTP 413)"
			return summary, fmt.Sprintf("%s: the commit request exceeded GitLab's body size cap (default 300 MB). "+
				"This provider batches every file change into one commit per apply, so split the files across multiple "+
				"`gitlabcommits_files` resources (for example with for_each), or on self-managed GitLab raise the "+
				"GITLAB_COMMITS_MAX_REQUEST_SIZE_BYTES limit. Body: %s", prefix, body)
		case 429:
			summary = "GitLab rate limit exceeded (HTTP 429)"
			retryAfter := resp.Response.Header.Get("Retry-After")
			if retryAfter == "" {
				retryAfter = "unknown"
			}
			return summary, fmt.Sprintf("%s: retry after %s seconds. Body: %s", prefix, retryAfter, body)
		default:
			return summary, fmt.Sprintf("%s: HTTP %d. Body: %s", prefix, status, body)
		}
	}

	return summary, fmt.Sprintf("%s: %s", prefix, err.Error())
}

// commitOptions assembles CreateCommitOptions from the shared resource fields
// and the list of actions for a single commit.
func commitOptions(m filesResourceModel, actions []*gitlab.CommitActionOptions) *gitlab.CreateCommitOptions {
	opts := &gitlab.CreateCommitOptions{
		Branch:        new(m.Branch.ValueString()),
		CommitMessage: new(m.CommitMessage.ValueString()),
		Actions:       actions,
	}
	if !m.AuthorEmail.IsNull() && !m.AuthorEmail.IsUnknown() {
		opts.AuthorEmail = new(m.AuthorEmail.ValueString())
	}
	if !m.AuthorName.IsNull() && !m.AuthorName.IsUnknown() {
		opts.AuthorName = new(m.AuthorName.ValueString())
	}
	return opts
}

// decodeRemoteContent returns raw bytes for a File response. GitLab's
// repository_files endpoints return base64 in practice; an empty Encoding
// is treated the same way (some self-hosted variants omit it). An "rot13"-
// or otherwise unknown encoding fails loudly rather than silently
// passing through whatever string came on the wire — a text file whose
// content is accidentally valid base64 should not be mis-decoded.
func decodeRemoteContent(f *gitlab.File) ([]byte, error) {
	// GetFile returns a nil *File (with no error) when the server sends a 2xx
	// JSON-null body; reject it rather than dereferencing f.Encoding.
	if f == nil {
		return nil, errors.New("GitLab returned an empty file object")
	}
	switch f.Encoding {
	case "", "base64":
		return base64.StdEncoding.DecodeString(f.Content)
	case "text":
		return []byte(f.Content), nil
	default:
		return nil, fmt.Errorf("unexpected encoding %q from GitLab", f.Encoding)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func buildID(project, branch string) string {
	return fmt.Sprintf("%s::%s", project, branch)
}
