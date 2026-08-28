// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
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

// refreshParallelism caps concurrent GitLab API calls within one resource
// operation (a Read, an adopt probe, a stampBlobs pass). It is a per-operation
// bound: with terraform's -parallelism (default 10) the process-wide
// concurrency is up to 10x this. 16 gives a 16x speedup on the documented
// fan-out use case (hundreds of files per resource) while client-go's
// RateLimit-Limit-derived limiter keeps the aggregate under GitLab's budget,
// and the retry layer handles 429s if a cap is hit anyway.
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
	client       *gitlab.Client
	locks        *branchLocks
	retryCommits bool
}

// resourceDeps is what the provider hands every resource through
// ResourceData: the shared client, the per-branch commit locks shared by all
// resource instances in this process, and whether the commit request may be
// retried at all (false when max_retries = 0: a per-request retry policy
// would otherwise bypass client-go's WithoutRetries).
type resourceDeps struct {
	client       *gitlab.Client
	locks        *branchLocks
	retryCommits bool
}

// branchLocks serialises commits per (project, branch) within one provider
// process. Terraform applies resource instances concurrently (-parallelism),
// and two CreateCommit calls racing on the same branch tip make GitLab reject
// the loser with "reference update: reference does not point to expected
// object"; holding the branch lock around the commit removes that race
// without merging or splitting commits, so every resource still lands exactly
// one. Writers outside this process are not covered and surface through
// apiErrorDiag.
type branchLocks struct {
	locks map[string]chan struct{}
	mu    sync.Mutex
}

func newBranchLocks() *branchLocks {
	return &branchLocks{locks: map[string]chan struct{}{}}
}

// acquire blocks until the (project, branch) lock is free or ctx is done and
// returns the matching release func. The key is the verbatim project_id, so
// resources sharing a branch must spell it the same way. release is
// idempotent so a caller can defer it for the error paths and still call it
// early once the critical section is over.
func (b *branchLocks) acquire(ctx context.Context, project, branch string) (func(), error) {
	key := buildID(project, branch)
	b.mu.Lock()
	ch, ok := b.locks[key]
	if !ok {
		ch = make(chan struct{}, 1)
		b.locks[key] = ch
	}
	b.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return sync.OnceFunc(func() { <-ch }), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// isCommitSHA reports whether s is a full SHA-1 or SHA-256 hex object id, the
// one shape create_branch_from can take besides a branch name.
func isCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
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
			"(typically per service) so each apply produces exactly one commit per service. " +
			"Commits are serialised per branch within one provider configuration, so for_each resources sharing one " +
			"branch never race on its tip, and the commit request is retried only on HTTP 429, never on 5xx, " +
			"so one apply can never land two commits.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "Composite identifier: \"<project_id>::<branch>\".",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": schema.StringAttribute{
				Description: "Numeric project ID or the plain project path (e.g. \"group/subgroup/project\"); do not URL-encode it, " +
					"the provider escapes it. Changing it forces replacement: with the default delete_on_destroy = true the " +
					"replacement first pushes a commit deleting every managed file from the old project and branch.",
				Required: true,
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
				Description: "Target branch. Must already exist, or set create_branch_from to materialise it. " +
					"Changing it forces replacement: with the default delete_on_destroy = true the replacement first pushes " +
					"a commit deleting every managed file from the old branch before creating them on the new one.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringNotEmpty(),
					stringBranchName(),
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
					"when the remote blob differs, so terraform plan reflects the real repository state. " +
					"With false, Read is a no-op: a file deleted out of band stays in state until detect_drift is " +
					"re-enabled and a refresh runs, and until then an update that removes it fails with GitLab's 400.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"delete_on_destroy": schema.BoolAttribute{
				Description: "If true (default), terraform destroy creates one commit that removes every managed file. " +
					"Set to false to keep files in place when the resource is removed from state. " +
					"Terraform does not evaluate configuration during destroy, so this value is read from the state " +
					"written by the last apply: a change made in HCL must be applied before terraform destroy honours it.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"adopt_existing": schema.BoolAttribute{
				Description: "If true (default), files that exist in the repository but are not yet in state are " +
					"adopted on the next apply: a create-action targeting an existing path is silently rewritten " +
					"as an update. When optimistic_lock is enabled, that adopt-update carries the file's current " +
					"commit, so a concurrent external modification is still detected instead of being overwritten. " +
					"An existing path whose content and mode already match the plan needs no action at all, so an apply " +
					"that only adopts identical files makes no commit and leaves commit_sha unset. " +
					"Required for terraform import to converge cleanly.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"create_branch_from": schema.StringAttribute{
				Description: "If set and `branch` does not yet exist, the provider creates it from this " +
					"branch name or full commit SHA (typically \"main\"; tags are not supported) together with " +
					"the first commit, as one push event (when adoption leaves nothing to commit, the branch is created on its own). " +
					"Only consulted by Create; once the branch exists, changing or removing this value is a " +
					"state-only no-op (no destroy / recreate). A branch created this way is not deleted by " +
					"terraform destroy; only the managed files are.",
				Optional: true,
				Validators: []validator.String{
					stringNotEmpty(),
					stringBranchName(),
				},
			},
			"optimistic_lock": schema.BoolAttribute{
				Description: "If true (default), update / delete / chmod actions send the file's last_commit_id to GitLab. " +
					"GitLab rejects the action with HTTP 400 if the file has been modified by anyone else since " +
					"this resource last touched it, preventing silent overwrites in concurrent pipelines. " +
					"Set to false to opt out (useful when an external process intentionally co-edits the same files). " +
					"Like delete_on_destroy, the destroy commit uses the value recorded by the last apply.",
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
					// Interior spaces and other characters are tolerated - git
					// accepts quite a lot; only traversal segments, NUL bytes,
					// empty segments, and leading whitespace are rejected.
					mapKeysValidRepoPath(),
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
								stringIsBase64(),
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
	deps, ok := req.ProviderData.(*resourceDeps)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *resourceDeps, got: %T. Please report this to the provider developers.", req.ProviderData),
		)
		return
	}
	r.client = deps.client
	r.locks = deps.locks
	r.retryCommits = deps.retryCommits
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

	project := plan.ProjectID.ValueString()
	branch := plan.Branch.ValueString()
	createFrom := plan.CreateBranchFrom.ValueString()

	branchExists, existsErr := r.branchExists(ctx, project, branch)
	if existsErr != nil {
		summary, detail := apiErrorDiag("checking branch", project, branch, existsErr)
		resp.Diagnostics.AddError(summary, detail)
		return
	}
	if !branchExists {
		if err := r.missingBranchPreflight(ctx, project, branch, createFrom); err != nil {
			summary, detail := apiErrorDiag("ensuring branch exists", project, branch, err)
			resp.Diagnostics.AddError(summary, detail)
			return
		}
	}

	paths := sortedKeys(plan.Files)
	useLock := plan.optimisticLock()
	// Adoption must be resolved against what the target branch will actually
	// contain: the branch itself when it exists, otherwise the ref it is
	// about to be created from - a managed path inherited from that ref must
	// become an adopt-update, not a create doomed to "already exists".
	var probes map[string]remoteProbe
	if plan.adoptExisting() {
		probeRef := branch
		if !branchExists {
			probeRef = createFrom
		}
		probes = r.probeRemote(ctx, project, probeRef, paths, true)
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
	plan.ID = types.StringValue(buildID(project, branch))

	// The lock covers branch materialisation and the commit: two instances
	// materialising the same branch must see each other's work, or the
	// second one would try to create a branch that by then exists. The
	// checks and probes above only read, so they ran unlocked; the branch is
	// re-checked here. Probes taken against create_branch_from stay valid
	// when the branch appeared meanwhile: it was created from that ref, and
	// another instance's files are never this resource's paths.
	release, lockErr := r.locks.acquire(ctx, project, branch)
	if lockErr != nil {
		resp.Diagnostics.AddError("Cancelled while waiting for the branch lock", lockErr.Error())
		return
	}
	defer release()
	if !branchExists {
		branchExists, existsErr = r.branchExists(ctx, project, branch)
		if existsErr != nil {
			summary, detail := apiErrorDiag("checking branch", project, branch, existsErr)
			resp.Diagnostics.AddError(summary, detail)
			return
		}
	}

	if len(actions) == 0 {
		// Every path was adopted with identical content and mode: nothing to
		// commit. A missing branch is still materialised, as a bare branch
		// creation (one push event, no commit).
		if !branchExists {
			if err := r.createBranch(ctx, project, branch, createFrom); err != nil {
				summary, detail := apiErrorDiag("ensuring branch exists", project, branch, err)
				resp.Diagnostics.AddError(summary, detail)
				return
			}
		}
		release()
		carryOver(plan.Files, nil, probes, nil)
		plan.CommitSHA = types.StringNull()
		tflog.Info(ctx, "GitLab files already match, no commit", map[string]any{
			"project_id": project,
			"branch":     branch,
		})
		resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
		return
	}

	opts := commitOptions(plan, actions)
	action := "creating commit"
	if !branchExists {
		// start_branch / start_sha make GitLab create the branch and land the
		// commit in one operation: a server-side rejection (push rule,
		// pre-receive hook) leaves no empty orphaned branch behind, and CI
		// sees one push event instead of a branch creation followed by a
		// commit. The commits API keeps the two apart where the branches API
		// took either as "ref".
		if isCommitSHA(createFrom) {
			opts.StartSHA = new(createFrom)
		} else {
			opts.StartBranch = new(createFrom)
		}
		action = fmt.Sprintf("creating branch %q from create_branch_from ref %q with the first commit", branch, createFrom)
	}

	tflog.Debug(ctx, "Creating GitLab files commit", map[string]any{
		"project_id":    project,
		"branch":        branch,
		"actions":       len(actions),
		"create_branch": !branchExists,
	})

	commit, _, err := r.client.Commits.CreateCommit(project, opts, r.commitRequestOptions(ctx)...)
	// stampBlobs probes the immutable commit just created, so it needs no
	// lock; release here rather than at the deferred return.
	release()
	if err != nil {
		summary, detail := apiErrorDiag(action, project, branch, err)
		resp.Diagnostics.AddError(summary, detail)
		return
	}
	// A 2xx response with a JSON-null body decodes to a nil *Commit with no
	// error, and an empty id is just as unusable; guard before dereferencing
	// so a hostile/buggy GitLab cannot panic the provider process after a
	// commit may already have landed. Returning without state is deliberate:
	// a Create that returns state on error is tainted by Terraform and
	// replaced, which would push a delete commit; a re-run converges through
	// adoption instead.
	if commit == nil || commit.ID == "" {
		resp.Diagnostics.AddError("GitLab returned no commit",
			"CreateCommit succeeded but the response contained no commit object; repository state is unknown. "+
				"Run `terraform plan` and apply again: with adopt_existing enabled (default) the retry adopts whatever the "+
				"first attempt committed instead of failing on existing files.")
		return
	}

	touched := touchedPaths(actions)
	carryOver(plan.Files, nil, probes, touched)
	resp.Diagnostics.Append(r.stampBlobs(ctx, project, plan.Files, commit.ID, touched)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.CommitSHA = types.StringValue(commit.ID)

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
			// client-go builds the metadata from response headers and reports
			// a missing X-Gitlab-Blob-Id as "": comparing that with state
			// would read as "unchanged" forever, so it is an error instead.
			if meta.BlobID == "" {
				return &pathError{path: p, err: errors.New("GitLab returned no blob_id in the metadata response")}
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
		if pe, ok := errors.AsType[*pathError](err); ok {
			summary, detail := apiErrorDiag(fmt.Sprintf("reading file %q", pe.path), project, branch, pe.err)
			resp.Diagnostics.AddError(summary, detail)
			return
		}
		resp.Diagnostics.AddError("Refresh failed", err.Error())
		return
	}

	// Every managed file 404-ing at once usually means the container vanished
	// (branch or project deleted out of band, or the token lost access -
	// GitLab answers 404 for all three). Silently emptying the files map would
	// strand the resource with state no apply can fix; drop it from state
	// instead so the next apply recreates everything from scratch. With no
	// files at all (right after import) the branch is the only thing to check.
	if len(paths) == 0 || allDropped(results) {
		_, _, err := r.client.Branches.GetBranch(project, branch, gitlab.WithContext(ctx))
		if err != nil {
			if errors.Is(err, gitlab.ErrNotFound) {
				resp.Diagnostics.AddWarning("Branch no longer exists",
					fmt.Sprintf("branch %q in project %q is gone (deleted out of band, project removed, or the token lost access); "+
						"removing the resource from state so the next apply can recreate it", branch, project))
				resp.State.RemoveResource(ctx)
				return
			}
			summary, detail := apiErrorDiag("checking branch after all managed files vanished", project, branch, err)
			resp.Diagnostics.AddError(summary, detail)
			return
		}
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

func allDropped(results []fileRefreshResult) bool {
	if len(results) == 0 {
		return false
	}
	for i := range results {
		if !results[i].drop {
			return false
		}
	}
	return true
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

	actions, probes, err := r.diffActions(ctx, plan, state)
	if err != nil {
		resp.Diagnostics.AddError("Error building actions", err.Error())
		return
	}

	if len(actions) == 0 {
		// Nothing to commit: keep computed fields from state, or from the
		// adopt probe for paths that turned out to already match.
		carryOver(plan.Files, state.Files, probes, nil)
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

	release, err := r.locks.acquire(ctx, plan.ProjectID.ValueString(), plan.Branch.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cancelled while waiting for the branch lock", err.Error())
		return
	}
	defer release()
	commit, _, err := r.client.Commits.CreateCommit(
		plan.ProjectID.ValueString(),
		commitOptions(plan, actions),
		r.commitRequestOptions(ctx)...,
	)
	release()
	if err != nil {
		summary, detail := apiErrorDiag("pushing update commit", plan.ProjectID.ValueString(), plan.Branch.ValueString(), err)
		resp.Diagnostics.AddError(summary, detail)
		return
	}
	// See Create: a JSON-null body decodes to a nil *Commit with no error.
	if commit == nil || commit.ID == "" {
		resp.Diagnostics.AddError("GitLab returned no commit",
			"CreateCommit succeeded but the response contained no commit object; repository state is unknown. Run `terraform plan` to reconcile.")
		return
	}

	touched := touchedPaths(actions)
	carryOver(plan.Files, state.Files, probes, touched)
	resp.Diagnostics.Append(r.stampBlobs(ctx, plan.ProjectID.ValueString(), plan.Files, commit.ID, touched)...)
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
	probes := r.probeRemote(ctx, project, branch, paths, false)

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
			Action:   new(gitlab.FileDelete),
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

	release, err := r.locks.acquire(ctx, project, branch)
	if err != nil {
		resp.Diagnostics.AddError("Cancelled while waiting for the branch lock", err.Error())
		return
	}
	defer release()
	_, _, err = r.client.Commits.CreateCommit(project, commitOptions(state, actions), r.commitRequestOptions(ctx)...)
	release()
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
	// Import records nothing but the id, so a typo would only surface as an
	// empty, unfixable resource at the next plan; check the branch now.
	exists, err := r.branchExists(ctx, project, branch)
	if err != nil {
		summary, detail := apiErrorDiag("checking the branch on import", project, branch, err)
		resp.Diagnostics.AddError(summary, detail)
		return
	}
	if !exists {
		resp.Diagnostics.AddError("Branch not found",
			fmt.Sprintf("branch %q does not exist in project %q, or the token cannot see the project (GitLab answers 404 for both); nothing to import", branch, project))
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
// Surrounding whitespace (a stray space in a copy-pasted import command) is
// trimmed rather than smuggled into the project or branch value.
func parseImportID(s string) (project, branch string, err error) {
	parts := strings.Split(s, "::")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("expected \"project_id::branch\", got %q", s)
	}
	project, branch = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if project == "" || branch == "" {
		return "", "", fmt.Errorf("expected \"project_id::branch\", got %q", s)
	}
	return project, branch, nil
}

// diffActions computes the minimal set of commit actions needed to make the
// repository match the plan, plus the adopt probes it took so the caller can
// stamp paths that needed no action. For files newly added in the plan, when
// adopt_existing is enabled, it prefers update over create if the path already
// exists in the repo (e.g. after terraform import), and emits nothing at all
// when the remote content already matches. When optimistic_lock is enabled,
// update / delete / chmod actions carry the file's previously-known
// last_commit_id so GitLab rejects the action if the file was concurrently
// modified.
func (r *filesResource) diffActions(ctx context.Context, plan, state filesResourceModel) ([]*gitlab.CommitActionOptions, map[string]remoteProbe, error) {
	actions := make([]*gitlab.CommitActionOptions, 0)
	useLock := plan.optimisticLock()

	// Deletes go first: GitLab applies a commit's actions in order against
	// one index, so a path turning from a file into a directory (or back)
	// only works when the old entry is gone before the new one is created.
	for _, p := range sortedKeys(state.Files) {
		if _, kept := plan.Files[p]; kept {
			continue
		}
		del := &gitlab.CommitActionOptions{
			Action:   new(gitlab.FileDelete),
			FilePath: new(p),
		}
		if useLock {
			if lcid := state.Files[p].LastCommitID.ValueString(); lcid != "" {
				del.LastCommitID = new(lcid)
			}
		}
		actions = append(actions, del)
	}

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
			probes = r.probeRemote(ctx, plan.ProjectID.ValueString(), plan.Branch.ValueString(), newPaths, true)
		}
	}

	for _, p := range sortedKeys(plan.Files) {
		pf := plan.Files[p]
		sf, exists := state.Files[p]

		if !exists {
			acts, err := adoptAwareActions(p, pf, probes[p], useLock)
			if err != nil {
				return nil, nil, fmt.Errorf("file %q: %w", p, err)
			}
			actions = append(actions, acts...)
			continue
		}

		lastCommitID := ""
		if useLock {
			lastCommitID = sf.LastCommitID.ValueString()
		}

		changed, err := contentChanged(pf, sf)
		if err != nil {
			return nil, nil, fmt.Errorf("file %q: %w", p, err)
		}
		if changed {
			a, err := buildAction(p, pf, gitlab.FileUpdate, lastCommitID)
			if err != nil {
				return nil, nil, fmt.Errorf("file %q: %w", p, err)
			}
			actions = append(actions, a)
		}

		planExec := pf.ExecuteFilemode.ValueBool()
		stateExec := sf.ExecuteFilemode.ValueBool()
		if planExec != stateExec {
			chmod := &gitlab.CommitActionOptions{
				Action:          new(gitlab.FileChmod),
				FilePath:        new(p),
				ExecuteFilemode: new(planExec),
			}
			if lastCommitID != "" {
				chmod.LastCommitID = new(lastCommitID)
			}
			actions = append(actions, chmod)
		}
	}

	return actions, probes, nil
}

// touchedPaths is the set of paths a commit's actions write to; every other
// managed path keeps the blob_id / last_commit_id it already had.
func touchedPaths(actions []*gitlab.CommitActionOptions) map[string]bool {
	out := make(map[string]bool, len(actions))
	for _, a := range actions {
		if a.FilePath != nil {
			out[*a.FilePath] = true
		}
	}
	return out
}

// carryOver fills blob_id / last_commit_id for every path not in skip: from
// state when the path was already managed, otherwise from the adopt probe (a
// path that already matched the plan), otherwise null. Paths in skip are left
// to stampBlobs, which reads them off the commit just created.
func carryOver(files, state map[string]fileModel, probes map[string]remoteProbe, skip map[string]bool) {
	for p, f := range files {
		if skip[p] {
			continue
		}
		if existing, ok := state[p]; ok {
			f.BlobID = existing.BlobID
			f.LastCommitID = existing.LastCommitID
		} else if probe := probes[p]; probe.exists {
			f.BlobID = stringOrNull(probe.blobID)
			f.LastCommitID = stringOrNull(probe.lastCommitID)
		} else {
			f.BlobID = types.StringNull()
			f.LastCommitID = types.StringNull()
		}
		files[p] = f
	}
}

func stringOrNull(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// remoteProbe is the result of a metadata probe for one path: whether the
// file exists at the ref and, if so, its blob_id, last_commit_id and exec bit,
// plus the content when the probe was asked for it (adoption compares it with
// the plan to avoid a no-op update commit). The lock token lets an
// adopt-update be guarded by optimistic_lock even though there is no prior
// state for the file. err is set for any failure other than a genuine 404 so
// callers can tell "absent" from "unknown": the adopt paths deliberately fall
// back to a plain update on it (a spurious create fails loudly at
// CreateCommit anyway), but Delete must not - skipping a file because a probe
// errored would let destroy report success while the file still exists.
type remoteProbe struct {
	err             error
	lastCommitID    string
	blobID          string
	content         []byte
	exists          bool
	executeFilemode bool
	hasContent      bool
}

// probeFile reports whether filePath is present at ref and returns its
// metadata, plus the decoded content when withContent is set. Only a genuine
// 404 maps to "absent"; any other failure is carried in err.
func (r *filesResource) probeFile(ctx context.Context, project, ref, filePath string, withContent bool) remoteProbe {
	meta, _, err := r.client.RepositoryFiles.GetFileMetaData(project, filePath, &gitlab.GetFileMetaDataOptions{
		Ref: new(ref),
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
	probe := remoteProbe{exists: true, blobID: meta.BlobID, lastCommitID: meta.LastCommitID, executeFilemode: meta.ExecuteFilemode}
	if !withContent {
		return probe
	}
	file, _, err := r.client.RepositoryFiles.GetFile(project, filePath, &gitlab.GetFileOptions{
		Ref: new(ref),
	}, gitlab.WithContext(ctx))
	if err != nil {
		probe.err = err
		return probe
	}
	raw, err := decodeRemoteContent(file)
	if err != nil {
		probe.err = err
		return probe
	}
	probe.content, probe.hasContent = raw, true
	return probe
}

// adoptAwareActions builds the action set for a path with no prior state: a
// plain create, or - when the path already exists remotely - an adopt-update
// carrying the probed lock token, or nothing at all when the remote content
// already matches the plan (the import round-trip must not produce a commit).
// The commits API honors execute_filemode only on create and chmod actions,
// so an adopt-update cannot set the exec bit itself; when the remote bit
// differs from the plan a companion chmod is emitted into the same commit,
// keeping one-commit-per-apply intact.
func adoptAwareActions(p string, f fileModel, probe remoteProbe, useLock bool) ([]*gitlab.CommitActionOptions, error) {
	op := gitlab.FileCreate
	lastCommitID := ""
	if probe.exists {
		op = gitlab.FileUpdate
		// Forward the probed last_commit_id under optimistic_lock so the
		// adopt-update still fails on a concurrent writer instead of blindly
		// overwriting it. A missing token would silently drop the guard, so
		// it is an error rather than an unlocked write.
		if useLock {
			if probe.lastCommitID == "" {
				return nil, errors.New("GitLab returned no last_commit_id for the existing file, so optimistic_lock cannot guard its adoption; retry, or set optimistic_lock = false")
			}
			lastCommitID = probe.lastCommitID
		}
	}
	var chmod *gitlab.CommitActionOptions
	if probe.exists && probe.executeFilemode != f.ExecuteFilemode.ValueBool() {
		chmod = &gitlab.CommitActionOptions{
			Action:          new(gitlab.FileChmod),
			FilePath:        new(p),
			ExecuteFilemode: new(f.ExecuteFilemode.ValueBool()),
		}
		if lastCommitID != "" {
			chmod.LastCommitID = new(lastCommitID)
		}
	}
	if probe.hasContent {
		raw, err := f.rawBytes()
		if err != nil {
			return nil, err
		}
		if bytes.Equal(raw, probe.content) {
			if chmod != nil {
				return []*gitlab.CommitActionOptions{chmod}, nil
			}
			return nil, nil
		}
	}
	a, err := buildAction(p, f, op, lastCommitID)
	if err != nil {
		return nil, err
	}
	actions := []*gitlab.CommitActionOptions{a}
	if chmod != nil {
		actions = append(actions, chmod)
	}
	return actions, nil
}

// probeRemote fans probeFile out across paths at refreshParallelism. The
// goroutines always return nil so Wait never fails; each path's outcome -
// including non-404 probe errors - travels in its remoteProbe's err field
// for the caller to interpret (Delete aborts on it, the adopt paths fall
// back to an update on purpose).
func (r *filesResource) probeRemote(ctx context.Context, project, ref string, paths []string, withContent bool) map[string]remoteProbe {
	out := make(map[string]remoteProbe, len(paths))
	if len(paths) == 0 {
		return out
	}
	probes := make([]remoteProbe, len(paths))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(refreshParallelism)
	for i, p := range paths {
		g.Go(func() error {
			probes[i] = r.probeFile(gctx, project, ref, p, withContent)
			return nil
		})
	}
	_ = g.Wait()
	for i, p := range paths {
		out[p] = probes[i]
	}
	return out
}

// branchExists reports whether branch is present, distinguishing a genuine
// 404 from transport/auth failures.
func (r *filesResource) branchExists(ctx context.Context, project, branch string) (bool, error) {
	_, _, err := r.client.Branches.GetBranch(project, branch, gitlab.WithContext(ctx))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gitlab.ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("checking branch %q: %w", branch, err)
}

// missingBranchPreflight validates that an absent branch can actually be
// materialised. A 404 on the project means the project itself is the problem
// (missing, or invisible to the token), not the branch; on a repository with
// zero commits every branch lookup 404s and no ref exists to branch from, so
// the create_branch_from advice would be a dead end. Both cases say what
// actually helps.
func (r *filesResource) missingBranchPreflight(ctx context.Context, project, branch, createFrom string) error {
	proj, _, err := r.client.Projects.GetProject(project, nil, gitlab.WithContext(ctx))
	if err != nil {
		if errors.Is(err, gitlab.ErrNotFound) {
			return fmt.Errorf("project %q does not exist or the token cannot see it (GitLab answers 404 for both); "+
				"check project_id and the token's scope and membership", project)
		}
		return fmt.Errorf("checking project %q: %w", project, err)
	}
	if proj != nil && proj.EmptyRepo {
		return fmt.Errorf("repository %q has no commits, so branch %q cannot exist and create_branch_from has no ref to start from; "+
			"create an initial commit first (for example initialize the project with a README)", project, branch)
	}
	if createFrom == "" {
		return fmt.Errorf("branch %q does not exist; set create_branch_from to materialise it", branch)
	}
	return nil
}

// createBranch materialises branch from createFrom without a commit. Only
// used when adoption found nothing to commit; otherwise start_branch on the
// first commit does both in one operation.
func (r *filesResource) createBranch(ctx context.Context, project, branch, createFrom string) error {
	_, _, err := r.client.Branches.CreateBranch(project, &gitlab.CreateBranchOptions{
		Branch: new(branch),
		Ref:    new(createFrom),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("creating branch %q from create_branch_from ref %q: %w", branch, createFrom, err)
	}
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

// contentChanged reports whether plan content differs from state without
// decoding in the common no-change case: equal text strings ARE the bytes,
// unequal text strings differ bytewise, and equal base64 strings decode to
// equal bytes. Only unequal base64 strings (two encodings with non-canonical
// trailing bits can still describe identical bytes) and a form switch between
// content and content_base64 fall back to a full bytewise comparison, so a
// cosmetic re-encoding never produces a spurious commit.
func contentChanged(pf, sf fileModel) (bool, error) {
	switch {
	case !pf.Content.IsNull() && !pf.Content.IsUnknown() && !sf.Content.IsNull():
		return pf.Content.ValueString() != sf.Content.ValueString(), nil
	case !pf.ContentBase64.IsNull() && !pf.ContentBase64.IsUnknown() && !sf.ContentBase64.IsNull():
		if pf.ContentBase64.ValueString() == sf.ContentBase64.ValueString() {
			return false, nil
		}
	}
	raw, err := pf.rawBytes()
	if err != nil {
		return false, err
	}
	stateRaw, err := sf.rawBytes()
	if err != nil {
		return false, err
	}
	return !bytes.Equal(raw, stateRaw), nil
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

// stampBlobs refreshes BlobID and LastCommitID for the paths a successful
// CreateCommit touched (every path when touched is nil). BlobID is pulled via
// parallel GetFileMetaData probes (HEAD-style) so state reflects what GitLab
// returns, including any future blob-id format (SHA-256 repos). Paths the
// commit did not touch are not probed at all; the caller carries their
// values over from state (carryOver), so a probe hiccup can never stamp this
// commit's SHA next to a file it never modified.
//
// Probes run at Ref = commitSHA (the commit just created), NOT at branch
// HEAD: a writer landing between our CreateCommit and the probe would
// otherwise have their blob_id and last_commit_id stamped into state next
// to OUR content, permanently blinding drift detection (Read sees the
// blob match) and handing the next locked apply a token that matches the
// racer's commit. Probing our own commit keeps state self-consistent; the
// racer is then caught by the next Read (their blob differs) and by the
// optimistic lock (our commit id no longer matches the file's last
// commit).
//
// Fail-soft covers three shapes: a probe error, a 2xx that carries no
// blob_id or last_commit_id header, and a blob_id longer than 256 bytes
// (generous ceiling above SHA-512 hex; anything longer is unexpected and
// treated as hostile). In all of them BlobID is left null, LastCommitID
// keeps the commitSHA first-pass stamp (correct for a touched file), and a
// warning is appended; the next Read repopulates both.
func (r *filesResource) stampBlobs(
	ctx context.Context,
	project string,
	files map[string]fileModel,
	commitSHA string,
	touched map[string]bool,
) diag.Diagnostics {
	var diagnostics diag.Diagnostics

	paths := make([]string, 0, len(files))
	for _, p := range sortedKeys(files) {
		if touched == nil || touched[p] {
			paths = append(paths, p)
		}
	}

	// First pass (serial): stamp LastCommitID from the known commit SHA and
	// reset BlobID to null so a probe failure leaves it null rather than stale.
	for _, p := range paths {
		f := files[p]
		f.LastCommitID = types.StringValue(commitSHA)
		f.BlobID = types.StringNull()
		files[p] = f
	}

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
				Ref: new(commitSHA),
			}, gitlab.WithContext(gctx))
			if err != nil {
				results[i].err = err
				return nil //nolint:nilerr // intentional: store per-file error, don't cancel other probes
			}
			if len(meta.BlobID) > maxBlobIDLen {
				results[i].err = fmt.Errorf("path %q: server returned blob_id of unexpected length %d (max %d)", p, len(meta.BlobID), maxBlobIDLen)
				return nil //nolint:nilerr // intentional: treat oversized blob_id as probe failure
			}
			if meta.BlobID == "" || meta.LastCommitID == "" {
				results[i].err = fmt.Errorf("path %q: metadata response carried no blob_id or last_commit_id", p)
				return nil //nolint:nilerr // intentional: a header-less 2xx is a failed probe, not a value to store
			}
			results[i].blobID = meta.BlobID
			results[i].lastCommitID = meta.LastCommitID
			return nil
		})
	}
	_ = g.Wait()

	// Second pass (serial): apply probe results, overwriting the first-pass
	// commitSHA with the per-file LastCommitID probed at Ref=commitSHA.
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
	// A diagnostic travels as a proto3 string, which must be valid UTF-8;
	// a proxy error page in another encoding would otherwise fail to marshal.
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= maxDiagBodyChars {
		return s
	}
	// Cut on a rune boundary for the same reason.
	cut := maxDiagBodyChars
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… (truncated, %d more chars)", len(s)-cut)
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

	// client-go collapses every 404 into the bare ErrNotFound sentinel (no
	// *ErrorResponse survives), so 404 must be recognised here - a status
	// switch below would never see it.
	if errors.Is(err, gitlab.ErrNotFound) {
		summary = "GitLab resource not found (HTTP 404)"
		return summary, fmt.Sprintf("%s: the project, branch, or file does not exist, or the token cannot see it "+
			"(GitLab answers 404 for missing access as well).", prefix)
	}

	if resp, ok := errors.AsType[*gitlab.ErrorResponse](err); ok && resp.Response != nil {
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
			// Gitaly refuses to move the ref when the branch tip changed between
			// reading it and writing the commit: "reference update: reference
			// does not point to expected object". Commits are serialised per
			// branch inside this process, so this means a writer outside this
			// terraform run.
			if strings.Contains(lower, "expected object") {
				summary = "Branch changed while the commit was being created"
				return summary, fmt.Sprintf("%s: another writer pushed to the branch while GitLab was building this commit, "+
					"so the ref update was refused and nothing was committed. This provider serialises its own commits per "+
					"branch within one provider configuration (each provider block runs in its own process), so the other "+
					"writer is another process: a different pipeline, a manual push, a bot, or a second provider block "+
					"(alias) targeting the same branch. Wait for it to finish and re-run terraform apply. Body: %s", prefix, body)
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
		case 301, 302, 303, 307, 308:
			// crossHostRedirectGuard stops the client from following an
			// off-host or https->http redirect; the 3xx then lands here.
			summary = fmt.Sprintf("Refused to follow a GitLab redirect (HTTP %d)", status)
			return summary, fmt.Sprintf("%s: GitLab answered with a redirect to %q. Redirects to another host or from https "+
				"to http are not followed because the request carries the API token; point base_url at the final GitLab "+
				"address. Body: %s", prefix, resp.Response.Header.Get("Location"), body)
		case 429:
			summary = "GitLab rate limit exceeded (HTTP 429)"
			h := resp.Response.Header
			switch {
			case h.Get("Retry-After") != "":
				return summary, fmt.Sprintf("%s: retry after %s seconds. Body: %s", prefix, h.Get("Retry-After"), body)
			case h.Get("RateLimit-ResetTime") != "":
				return summary, fmt.Sprintf("%s: the limit resets at %s. Body: %s", prefix, h.Get("RateLimit-ResetTime"), body)
			case h.Get("RateLimit-Reset") != "":
				return summary, fmt.Sprintf("%s: the limit resets at unix time %s. Body: %s", prefix, h.Get("RateLimit-Reset"), body)
			default:
				// GitLab's per-endpoint limits answer without any rate-limit
				// header, unlike the Rack::Attack throttles.
				return summary, fmt.Sprintf("%s: no rate-limit headers were sent, which is how GitLab's per-endpoint limits "+
					"answer (on GitLab.com: commit requests over 20 MB are limited to 3 per 30 s and reads of blobs over 10 MB "+
					"to 5 per minute; self-managed instances configure their own). Raise max_retries / retry_wait_min_ms, or "+
					"split the bundle across resources. Body: %s", prefix, body)
			}
		default:
			if status >= 500 {
				// A commit request is deliberately never replayed on 5xx (see
				// commitRetryPolicy), so the user has to find out whether it
				// landed; reads were already retried by the client.
				summary = fmt.Sprintf("GitLab server error (HTTP %d)", status)
				return summary, fmt.Sprintf("%s: GitLab did not complete the request. A commit request is not retried by "+
					"this provider (a replay could land a second commit), so if this was one the commit may or may not "+
					"have landed: run `terraform plan` to see the repository state before applying again. Body: %s", prefix, body)
			}
			return summary, fmt.Sprintf("%s: HTTP %d. Body: %s", prefix, status, body)
		}
	}

	return summary, fmt.Sprintf("%s: %s", prefix, err.Error())
}

// commitRequestOptions returns the request options for POST /repository/commits:
// the context plus, when retries are enabled, the commit-specific retry policy.
func (r *filesResource) commitRequestOptions(ctx context.Context) []gitlab.RequestOptionFunc {
	opts := []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)}
	if r.retryCommits {
		opts = append(opts, gitlab.WithRequestRetry(commitRetryPolicy))
	}
	return opts
}

// commitRetryPolicy replaces client-go's default retry check for the commit
// request only. The default retries every 5xx regardless of method, and a
// 502/504 from a proxy after GitLab already landed the commit would replay
// the POST and produce a second commit for the same apply. Retry only a 429
// (rejected before processing) and failures that provably happened before
// anything reached the wire (DNS, dial); everything else fails loudly and the
// user reconciles with terraform plan.
func commitRetryPolicy(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		if dnsErr, ok := errors.AsType[*net.DNSError](err); ok {
			return !dnsErr.IsNotFound, nil
		}
		if opErr, ok := errors.AsType[*net.OpError](err); ok {
			return opErr.Op == "dial", nil
		}
		// net/http reports a handshake timeout as a plain error string; the
		// handshake precedes the request body, so it is pre-wire as well.
		if strings.Contains(err.Error(), "net/http: TLS handshake timeout") {
			return true, nil
		}
		return false, nil
	}
	return resp.StatusCode == http.StatusTooManyRequests, nil
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
	slices.Sort(keys)
	return keys
}

func buildID(project, branch string) string {
	return fmt.Sprintf("%s::%s", project, branch)
}
