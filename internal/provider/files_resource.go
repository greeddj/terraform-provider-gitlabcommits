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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
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
// ResourceData: the shared client, the per-branch commit locks and path
// claims shared by all resource instances in this process, and whether the
// commit request may be retried at all (false when max_retries = 0: a
// per-request retry policy would otherwise bypass client-go's
// WithoutRetries).
type resourceDeps struct {
	client       *gitlab.Client
	locks        *branchLocks
	retryCommits bool
}

// branchLocks serialises commits per (project, branch) within one provider
// process and records the paths resources claim there. Terraform applies
// resource instances concurrently (-parallelism), and two CreateCommit calls
// racing on the same branch tip make GitLab reject the loser with "reference
// update: reference does not point to expected object"; holding the branch
// lock around the commit removes that race without merging or splitting
// commits, so every resource still lands exactly one. Writers outside this
// process are not covered and surface through apiErrorDiag.
//
// A claim is a path a resource creates or adopts: Create claims every path
// it manages, Update the paths it adds. One apply can hand a path from one
// resource to another (a file moved between two bundles, an address renamed
// without a moved block, a replacement under create_before_destroy), and
// Terraform runs the giver's delete and the receiver's adoption either
// unordered or, under create_before_destroy, adoption first; an adoption of
// identical content makes no commit, so the delete would still land on its
// old last_commit_id. So the giver leaves out the delete of a claimed path
// (commitLocked), and the receiver probes only after passing through the
// branch lock once (claimPaths): a delete commit already in flight lands
// before the probe, and any later one sees the claim. The claims also keep a
// created file from replacing a directory that holds a claimed path
// (createsOverClaims). Claims live in memory for the life of the process
// and are never released; a failed Create keeps its claim, so the file
// stays, which is the safe direction. A later run, a re-run of a failed
// apply included, cannot see them. projectIDs caches the numeric ID behind
// each project_id path spelling, looked up only when a claim meets a delete
// or a create on one branch under different spellings (an all-digit
// spelling is its own ID).
type branchLocks struct {
	locks      map[string]chan struct{}
	claims     map[claimKey]map[string]bool
	projectIDs map[string]int64
	mu         sync.Mutex
}

// claimKey is a path on a branch; claims maps it to the project_id
// spellings it was claimed under.
type claimKey struct {
	branch, path string
}

func newBranchLocks() *branchLocks {
	return &branchLocks{
		locks:      map[string]chan struct{}{},
		claims:     map[claimKey]map[string]bool{},
		projectIDs: map[string]int64{},
	}
}

// claim records paths as claimed on (project, branch).
func (b *branchLocks) claim(project, branch string, paths []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range paths {
		k := claimKey{branch: branch, path: p}
		if b.claims[k] == nil {
			b.claims[k] = map[string]bool{}
		}
		b.claims[k][project] = true
	}
}

// claimants returns the project_id spellings path is claimed under on
// branch, sorted.
func (b *branchLocks) claimants(branch, path string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return sortedKeys(b.claims[claimKey{branch: branch, path: path}])
}

// claimedInside maps every path claimed on branch, under any project_id
// spelling, that lies inside one of the directories dirs to that directory.
func (b *branchLocks) claimedInside(branch string, dirs map[string]bool) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k := range b.claims {
		if k.branch != branch {
			continue
		}
		for dir := k.path; ; {
			i := strings.LastIndexByte(dir, '/')
			if i < 0 {
				break
			}
			if dir = dir[:i]; dirs[dir] {
				out[k.path] = dir
				break
			}
		}
	}
	return out
}

func (b *branchLocks) projectID(project string) (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.projectIDs[project]
	return id, ok
}

func (b *branchLocks) setProjectID(project string, id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.projectIDs[project] = id
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

// claimPaths records paths as claimed on (project, branch), then takes the
// branch lock and releases it at once, so a delete commit another resource
// has in flight on the branch lands before the caller probes the paths, and
// a delete that takes the lock afterwards sees the claim.
func (b *branchLocks) claimPaths(ctx context.Context, project, branch string, paths []string) error {
	b.claim(project, branch, paths)
	release, err := b.acquire(ctx, project, branch)
	if err != nil {
		return err
	}
	release()
	return nil
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
			"so one apply can never land two commits. Within one run of one provider configuration, no resource " +
			"deletes a file on its branch after another resource created or adopted it there: that delete is dropped " +
			"with a warning, so a file can move from one resource to another in one apply, and a replacement under " +
			"create_before_destroy keeps the files the new object took over. A delete commit already in flight is " +
			"waited for only when both resources spell project_id the same way. Later runs are not covered, a re-run " +
			"of a failed apply included.",
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
					"the provider escapes it. Changing it forces replacement, and so do respelling the same project (numeric ID " +
					"vs path) and a value that is unknown at plan time, even one that turns out unchanged. Without " +
					"create_before_destroy the old object is destroyed first: with the default delete_on_destroy = true that " +
					"pushes a commit deleting every managed file from the old project and branch before they are created again. " +
					"Under create_before_destroy the new object is created first, and when it targets the same project and " +
					"branch (a respelling, or a value that turned out unchanged) the old object's destroy leaves in place " +
					"every file the new object adopted or wrote. Keep it known at plan time (literals, variables), or apply " +
					"delete_on_destroy = false first, which makes such a replacement commit-free.",
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
				Description: "Target branch. Must already exist, or set create_branch_from to materialise it; in a repository " +
					"with no commits yet, leave create_branch_from unset and the first commit creates the branch. " +
					"Changing it forces replacement, and so does a value that is unknown at plan time, even one that turns " +
					"out unchanged. Without create_before_destroy the old object is destroyed first: with the default " +
					"delete_on_destroy = true that pushes a commit deleting every managed file from the old branch before " +
					"they are created on the new one. Under create_before_destroy the new object is created first, and when " +
					"the branch is in fact unchanged the old object's destroy leaves in place every file the new object " +
					"adopted or wrote. Keep it known at plan time (literals, variables), or apply delete_on_destroy = false " +
					"first, which makes such a replacement commit-free.",
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
					"With false, Read is a no-op and state keeps what the last apply recorded: under optimistic_lock an " +
					"update, chmod or delete of a file changed out of band fails with GitLab's 400 on every apply, and a " +
					"file deleted out of band stays in state (an update that removes it from files probes the path first " +
					"and drops the delete). A refresh reads the value recorded in state, not the configuration, and a " +
					"failed apply keeps that value, so a new detect_drift value only affects refreshes once an apply has " +
					"recorded it. To catch up with the repository, set detect_drift = true with files as last applied " +
					"and apply (this makes no commit), then put back any change you were applying and plan again: that " +
					"plan compares files with the branch, so with files still as last applied it would revert what " +
					"changed there.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"delete_on_destroy": schema.BoolAttribute{
				Description: "If true (default), terraform destroy creates one commit that removes every managed file. " +
					"Files already removed out of band are skipped; when none is left, destroy makes no commit and says " +
					"so in a warning. A file another gitlabcommits_files resource adopted or wrote in the same run is kept, " +
					"with a warning naming it. Set to false to keep files in place when the resource is removed from state. " +
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
					"that only adopts identical files makes no commit and leaves commit_sha unset. A path that cannot be " +
					"read for adoption fails the apply before anything is committed. " +
					"Required for terraform import to converge cleanly.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"create_branch_from": schema.StringAttribute{
				Description: "If set and `branch` does not yet exist, the provider creates it from this " +
					"branch name or full commit SHA (typically \"main\"; tags are not supported) together with " +
					"the first commit, as one push event (when adoption leaves nothing to commit, the branch is created on its own). " +
					"A branch name is resolved to its head commit once, and both adoption and the new branch use that " +
					"commit, so a commit pushed to the source branch in the meantime, by another resource in the same " +
					"apply included, is not part of the new branch. Must be unset in a repository with no commits yet. " +
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
					"Without the token the provider probes each path before a delete or chmod, because GitLab would " +
					"otherwise apply the action to whatever sits at the path, a directory included. " +
					"The first commit of a branch created from create_branch_from carries no token: GitLab would check " +
					"it against the default branch rather than the commit the branch starts from, which the provider " +
					"has just read and which cannot change. " +
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
				Description: "Map of repository_path -> file definition. The map key is the path inside the repo. " +
					"A new path where the branch holds a directory fails the apply, since the file would replace the " +
					"directory with everything in it; the one exception is a directory holding only files this resource " +
					"manages and drops from the map in the same apply. A new path also fails when another resource in the " +
					"same apply adds or adopts a file inside a directory of that name.",
				Required: true,
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
								"Deliver secrets via SealedSecrets / ExternalSecrets / Vault and reference them from the managed file. " +
								"When the file drifts to bytes that are not valid UTF-8, which content cannot hold, a refresh " +
								"records them in content_base64 instead, with a warning: the next apply restores this content, " +
								"and switching the file to content_base64 keeps the new bytes.",
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
// "create" to "update" so we don't fail with "file already exists". Every
// path is claimed before it is probed (see branchLocks), so no other
// resource in this run deletes a file Create adopted or wrote, short of a
// delete commit already in flight under another project_id spelling.
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
	// base is the commit a missing branch is created from, resolved once so
	// the probes and the new branch read the same tree even when
	// create_branch_from moves meanwhile; it stays "" on an empty repository.
	var base string
	if !branchExists {
		var err error
		if base, err = r.missingBranchPreflight(ctx, project, branch, createFrom); err != nil {
			summary, detail := apiErrorDiag("ensuring branch exists", project, branch, err)
			resp.Diagnostics.AddError(summary, detail)
			return
		}
	}

	paths := sortedKeys(plan.Files)
	if err := r.locks.claimPaths(ctx, project, branch, paths); err != nil {
		resp.Diagnostics.AddError("Cancelled while waiting for the branch lock", err.Error())
		return
	}
	// Adoption must be resolved against what the target branch will actually
	// contain: the branch itself when it exists, otherwise the commit it is
	// about to be created from - a managed path inherited from there must
	// become an adopt-update, not a create doomed to "already exists".
	probeRef := branch
	if !branchExists {
		probeRef = base
	}
	actions, probes, diags := r.createActions(ctx, plan, probeRef, paths)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(buildID(project, branch))

	// The lock covers branch materialisation and the commit: two instances
	// materialising the same branch must see each other's work, or the
	// second one would try to create a branch that by then exists. The
	// checks and probes above only read, so they ran unlocked; the branch is
	// re-checked here. Probes of an existing target branch stay valid
	// meanwhile as far as this process goes: they ran after the pass through
	// the lock in claimPaths, so a delete commit of these paths by another
	// resource had already landed, and a later one leaves the claimed paths
	// alone. Probes of base read an immutable commit. A branch that appeared
	// meanwhile (another instance on it got the lock first, the usual case
	// for many resources on one new branch) is probed again under the lock,
	// since it already holds that instance's commit. A directory another
	// resource fills after the unlocked directory check is caught by the
	// claims, checked under the lock as well.
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
		if branchExists {
			actions, probes, diags = r.createActions(ctx, plan, branch, paths)
			resp.Diagnostics.Append(diags...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
	}
	resp.Diagnostics.Append(r.createsOverClaims(ctx, project, branch, actions)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if len(actions) == 0 {
		// Every path was adopted with identical content and mode: nothing to
		// commit. A missing branch is still materialised, as a bare branch
		// creation (one push event, no commit). base is never "" here: on an
		// empty repository every path is a create.
		if !branchExists {
			if err := r.createBranch(ctx, project, branch, base); err != nil {
				// client-go replays the POST after a 5xx, and a replay of a
				// creation GitLab had already carried out answers "Branch
				// already exists". The lock keeps every other resource in
				// this process off the branch, so a branch that exists now is
				// the one this request created.
				if found, checkErr := r.branchExists(ctx, project, branch); checkErr != nil || !found {
					summary, detail := apiErrorDiag("ensuring branch exists", project, branch, err)
					resp.Diagnostics.AddError(summary, detail)
					return
				}
				tflog.Warn(ctx, "Branch creation reported an error, but the branch exists", map[string]any{
					"project_id": project,
					"branch":     branch,
					"error":      err.Error(),
				})
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
	if !branchExists && base == "" {
		// On an empty repository the commit carries the branch alone: GitLab
		// makes it the root commit and creates the branch with it.
		action = fmt.Sprintf("creating branch %q with the first commit of the empty repository", branch)
	} else if !branchExists {
		// start_sha makes GitLab create the branch and land the commit in one
		// operation: a server-side rejection (push rule, pre-receive hook)
		// leaves no empty orphaned branch behind, and CI sees one push event
		// instead of a branch creation followed by a commit.
		opts.StartSHA = new(base)
		// With start_sha and no start_branch, GitLab checks last_commit_id
		// against the default branch rather than base. The probes read the
		// immutable base, so a token guards nothing there and would only
		// fail on a path the default branch changed since.
		for _, a := range actions {
			a.LastCommitID = nil
		}
		action = fmt.Sprintf("creating branch %q from %s with the first commit", branch, describeBase(createFrom, base))
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
		"actions":    len(actions),
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
			// A missing blob_id would compare as "unchanged" forever, so an
			// unusable id is an error rather than a value to compare.
			if err = checkFileIDs(meta); err != nil {
				return &pathError{path: p, err: err}
			}
			results[i].metaLastCommitID = meta.LastCommitID
			if meta.BlobID == f.BlobID.ValueString() &&
				meta.ExecuteFilemode == f.ExecuteFilemode.ValueBool() {
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

	// A file that answered 404 is dropped only once the branch lookup has
	// answered too, since a Gitaly failure can reach the Files API as a 404
	// (see absentPaths); dropping it on that alone would let a destroy that
	// refreshed first leave the file in the repository. Every managed file
	// 404-ing at once on a branch that is gone as well usually means the
	// container vanished (branch or project deleted out of band, or the
	// token lost access - GitLab answers 404 for all three). Silently
	// emptying the files map would strand the resource with state no apply
	// can fix; drop it from state instead so the next apply recreates
	// everything from scratch. With no files at all (right after import) the
	// branch is the only thing to check.
	if len(paths) == 0 || slices.ContainsFunc(results, func(res fileRefreshResult) bool { return res.drop }) {
		found, err := r.branchExists(ctx, project, branch)
		if err != nil {
			if len(paths) == 0 {
				resp.Diagnostics.AddError(apiErrorDiag("checking the branch", project, branch, err))
				return
			}
			summary, detail := apiErrorDiag("checking the branch after a managed file answered 404", project, branch, err)
			resp.Diagnostics.AddError(summary, detail+gitaly404Note+" State was left unchanged.")
			return
		}
		if !found && (len(paths) == 0 || allDropped(results)) {
			resp.Diagnostics.AddWarning("Branch no longer exists",
				fmt.Sprintf("branch %q in project %q is gone (deleted out of band, project removed, or the token lost access); "+
					"removing the resource from state so the next apply can recreate it", branch, project))
			resp.State.RemoveResource(ctx)
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
			// Took the metadata-only branch (blob unchanged) - refresh
			// last_commit_id only when it has moved (a delete-then-re-add with
			// identical content would otherwise stale the optimistic-lock token
			// in state). A nil *File from a drifted blob is surfaced as an error
			// in the probe above, so it never reaches here.
			if res.metaLastCommitID != f.LastCommitID.ValueString() {
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
		if err := checkFileIDs(res.file); err != nil {
			// Leave blob_id unset, and take last_commit_id from the metadata
			// response, rather than persisting an unusable value. A server
			// that keeps returning one makes every Read re-fetch content (the
			// null we store never equals the HEAD blob, so drift never
			// settles), but Read never commits or persists a wrong value, so
			// the only cost is repeated GETs against a misbehaving server.
			resp.Diagnostics.AddWarning("Ignoring unusable file ids",
				fmt.Sprintf("file %q: %s; leaving blob_id unset", p, err))
			f.BlobID = types.StringNull()
			f.LastCommitID = types.StringValue(res.metaLastCommitID)
		} else {
			f.BlobID = types.StringValue(res.file.BlobID)
			f.LastCommitID = types.StringValue(res.file.LastCommitID)
		}
		f.ExecuteFilemode = types.BoolValue(res.file.ExecuteFilemode)
		if misfit := setRemoteContent(&f, raw); misfit != "" {
			resp.Diagnostics.AddWarning("Remote file recorded as content_base64",
				fmt.Sprintf("file %q is managed through `content`, but its content on branch %q %s, which `content` cannot "+
					"hold, so state records it in `content_base64`. The next apply restores the configured `content`. To "+
					"keep the new bytes instead, manage the file through `content_base64`.", p, branch, misfit))
		}
		state.Files[p] = f
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// setRemoteContent stores raw in f in the form f is managed through. Bytes
// the text form cannot hold (see textContentMisfit) go to content_base64
// instead, with content null, and misfit says why. Read never sees the
// configuration, so an error there would block every refresh whatever the
// file is switched to; content_base64 keeps the bytes exact, and the next
// plan shows the configured content replacing them.
func setRemoteContent(f *fileModel, raw []byte) (misfit string) {
	if f.ContentBase64.IsNull() {
		if misfit = textContentMisfit(raw); misfit == "" {
			f.Content = types.StringValue(string(raw))
			return ""
		}
		f.Content = types.StringNull()
	}
	f.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString(raw))
	return misfit
}

// textContentMisfit says why raw cannot be held in the text `content`
// attribute, or returns "" when it can. cty replaces invalid UTF-8 with
// U+FFFD, so such bytes stored as text would be corrupted in state and the
// diff could never converge.
func textContentMisfit(raw []byte) string {
	if !utf8.Valid(raw) {
		return "is not valid UTF-8"
	}
	return ""
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

// fileRefreshResult is the per-file outcome of a parallel refresh probe:
// drop for a file gone at the remote, otherwise the last_commit_id of the
// metadata response, plus the file when its blob drifted and the content
// was pulled.
type fileRefreshResult struct {
	file             *gitlab.File // non-nil iff blob drifted and content was pulled
	metaLastCommitID string       // set iff the file was found
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
// commit. If nothing changed, no commit is produced. The paths it adds are
// claimed before they are probed, and a delete of a path another resource
// in this run claimed is left out (see branchLocks).
func (r *filesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state filesResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	project := plan.ProjectID.ValueString()
	branch := plan.Branch.ValueString()

	if added := addedPaths(plan, state); len(added) > 0 {
		if err := r.locks.claimPaths(ctx, project, branch, added); err != nil {
			resp.Diagnostics.AddError("Cancelled while waiting for the branch lock", err.Error())
			return
		}
	}

	actions, probes, diags := r.diffActions(ctx, plan, state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	actions, _, diags = r.probeUnguarded(ctx, project, branch, actions, probes, !state.detectDrift(), "before the update commit")
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	var commit *gitlab.Commit
	var err error
	if len(actions) > 0 {
		tflog.Debug(ctx, "Updating GitLab files commit", map[string]any{
			"project_id": project,
			"branch":     branch,
			"actions":    len(actions),
		})
		commit, actions, diags, err = r.commitLocked(ctx, plan, actions)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err != nil {
			summary, detail := commitErrorDiag("pushing update commit", project, branch, err)
			resp.Diagnostics.AddError(summary, detail+unrefreshedNote(state, err, false))
			return
		}
	}

	if len(actions) == 0 {
		// Nothing was committed: keep computed fields from state, or from the
		// adopt probe for paths that turned out to already match.
		carryOver(plan.Files, state.Files, probes, nil)
		plan.CommitSHA = state.CommitSHA
		plan.ID = state.ID
		resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
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
	resp.Diagnostics.Append(r.stampBlobs(ctx, project, plan.Files, commit.ID, touched)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.CommitSHA = types.StringValue(commit.ID)
	plan.ID = types.StringValue(buildID(project, branch))

	tflog.Info(ctx, "GitLab files commit pushed", map[string]any{
		"project_id": project,
		"branch":     branch,
		"commit_sha": commit.ID,
		"actions":    len(actions),
	})

	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// Delete pushes one commit that removes every managed file; files already
// gone are skipped, so destroy is idempotent against out-of-band cleanup.
// A delete that carries a last_commit_id goes out without a probe: GitLab
// checks that token itself and rejects the whole commit when the file was
// changed, removed or replaced out of band, and a rejected commit lands
// nothing. Only a rejection (HTTP 400, or 404 when the project itself is
// gone) makes Delete probe the paths and retry once without the ones
// already gone, so at most one commit lands. A delete without a token is
// probed up front (see probeUnguarded). A path another resource in this run
// adopted or wrote is left in place (see branchLocks). Disabled by setting
// delete_on_destroy = false.
func (r *filesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state filesResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !state.deleteOnDestroy() || len(state.Files) == 0 {
		return
	}

	project := state.ProjectID.ValueString()
	branch := state.Branch.ValueString()
	useLock := state.optimisticLock()

	actions := make([]*gitlab.CommitActionOptions, 0, len(state.Files))
	for _, p := range sortedKeys(state.Files) {
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

	actions, branchFound, diags := r.probeUnguarded(ctx, project, branch, actions, nil, false, "before destroy")
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(actions) == 0 {
		resp.Diagnostics.AddWarning(nothingDeleted(project, branch, branchFound, false))
		return
	}

	_, sent, diags, err := r.commitLocked(ctx, state, actions)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() || err == nil {
		return
	}
	// GitLab answers a 400 (a file changed, removed or replaced out of band,
	// or the branch gone) and a 404 (the project gone, or invisible to the
	// token) before it writes anything, so only those are worth a probe; a
	// 5xx may have landed, and a 403 or a cancelled lock wait will not
	// change on a retry.
	if !hasStatus(err, http.StatusBadRequest) && !errors.Is(err, gitlab.ErrNotFound) {
		summary, detail := commitErrorDiag("pushing destroy commit", project, branch, err)
		resp.Diagnostics.AddError(summary, detail+unrefreshedNote(state, err, true))
		return
	}

	paths := make([]string, 0, len(sent))
	for _, a := range sent {
		paths = append(paths, *a.FilePath)
	}
	absent, branchFound, diags := r.absentPaths(ctx, project, branch, paths, "after GitLab rejected the destroy commit")
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(absent) == 0 {
		// Every file is still there, so the rejection was about something
		// else (a concurrent edit, a push rule, another writer on the branch).
		summary, detail := apiErrorDiag("pushing destroy commit", project, branch, err)
		resp.Diagnostics.AddError(summary, detail+unrefreshedNote(state, err, true))
		return
	}
	keptForOthers := len(sent) < len(actions)
	actions = withoutPaths(sent, absent)
	if len(actions) == 0 {
		resp.Diagnostics.AddWarning(nothingDeleted(project, branch, branchFound, keptForOthers))
		return
	}
	_, _, diags, err = r.commitLocked(ctx, state, actions)
	resp.Diagnostics.Append(diags...)
	if err != nil {
		summary, detail := commitErrorDiag("pushing destroy commit without the files already gone", project, branch, err)
		resp.Diagnostics.AddError(summary, detail+unrefreshedNote(state, err, true))
	}
}

// nothingDeleted is the warning for a destroy that found every managed file
// already gone and so made no commit. keptForOthers says some were never
// looked for, since they were left in place for another resource.
func nothingDeleted(project, branch string, branchFound, keptForOthers bool) (string, string) {
	lead := ""
	if keptForOthers {
		lead = "apart from the files left in place for another resource, "
	}
	if branchFound {
		return "Nothing left to delete", lead + fmt.Sprintf("none of the managed files exists on branch %q in project %q any more "+
			"(removed out of band), so destroy made no commit", branch, project)
	}
	return "Nothing left to delete", lead + fmt.Sprintf("no managed file is visible on branch %q in project %q, and neither is the branch "+
		"(deleted out of band, project removed, or the token lost access; GitLab answers 404 for all three), so destroy made "+
		"no commit. If the token lost access, the files may still exist in the repository.", branch, project)
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
// repository match the plan, plus the probes of the paths the plan adds so
// the caller can stamp paths that needed no action. For files newly added in
// the plan, when adopt_existing is enabled, it prefers update over create if
// the path already exists in the repo (e.g. after terraform import), and
// emits nothing at all when the remote content already matches; a path that
// is created must not have a directory in its place (see probeAdded). When
// optimistic_lock is enabled, update / delete / chmod actions carry the
// file's previously-known last_commit_id so GitLab rejects the action if the
// file was concurrently modified. Every failure is an error on the file it
// concerns, and nothing is committed.
func (r *filesResource) diffActions(ctx context.Context, plan, state filesResourceModel) ([]*gitlab.CommitActionOptions, map[string]remoteProbe, diag.Diagnostics) {
	actions := make([]*gitlab.CommitActionOptions, 0)
	useLock := plan.optimisticLock()

	// Deletes go first: GitLab applies a commit's actions in order against
	// one index, so a directory of managed files can turn into a file (and a
	// file into a directory) within one commit.
	deleted := map[string]bool{}
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
		deleted[p] = true
	}

	// Probe new-in-plan paths in parallel. The post-import path (state.Files
	// empty, plan.Files large) hits this with every managed file marked
	// "new", so a sequential probe per path would dominate the apply latency.
	var probes map[string]remoteProbe
	if added := addedPaths(plan, state); len(added) > 0 {
		var diags diag.Diagnostics
		probes, diags = r.probeAdded(ctx, plan, plan.Branch.ValueString(), added, deleted)
		if diags.HasError() {
			return nil, nil, diags
		}
	}

	for _, p := range sortedKeys(plan.Files) {
		pf := plan.Files[p]
		sf, exists := state.Files[p]

		if !exists {
			acts, err := adoptAwareActions(p, pf, probes[p], useLock)
			if err != nil {
				return nil, nil, fileActionDiags(p, err)
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
			return nil, nil, fileActionDiags(p, err)
		}
		if changed {
			a, err := buildAction(p, pf, gitlab.FileUpdate, lastCommitID)
			if err != nil {
				return nil, nil, fileActionDiags(p, err)
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

// createActions builds Create's actions: the paths are probed at ref (see
// probeAdded) and each becomes a create, an adopt-update or nothing (see
// adoptAwareActions). An empty ref is an empty repository, where nothing
// exists to probe or adopt.
func (r *filesResource) createActions(ctx context.Context, plan filesResourceModel, ref string, paths []string) ([]*gitlab.CommitActionOptions, map[string]remoteProbe, diag.Diagnostics) {
	var probes map[string]remoteProbe
	if ref != "" {
		var diags diag.Diagnostics
		probes, diags = r.probeAdded(ctx, plan, ref, paths, nil)
		if diags.HasError() {
			return nil, nil, diags
		}
	}
	actions := make([]*gitlab.CommitActionOptions, 0, len(paths))
	for _, p := range paths {
		acts, err := adoptAwareActions(p, plan.Files[p], probes[p], plan.optimisticLock())
		if err != nil {
			return nil, nil, fileActionDiags(p, err)
		}
		actions = append(actions, acts...)
	}
	return actions, probes, nil
}

// fileActionDiags reports a file that cannot be turned into a commit action;
// Create and Update share it so one cause reads the same in both.
func fileActionDiags(p string, err error) diag.Diagnostics {
	return diag.Diagnostics{diag.NewAttributeErrorDiagnostic(path.Root("files").AtMapKey(p),
		"Cannot build the commit action for a file", fmt.Sprintf("file %q: %s. Nothing was committed.", p, err))}
}

// addedPaths lists, sorted, the paths the plan manages and state does not:
// the paths an Update creates or adopts.
func addedPaths(plan, state filesResourceModel) []string {
	var added []string
	for _, p := range sortedKeys(plan.Files) {
		if _, ok := state.Files[p]; !ok {
			added = append(added, p)
		}
	}
	return added
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

// remoteProbe is the result of probing one path: whether the file exists at
// the ref and, if so, its blob_id, last_commit_id and exec bit, plus the
// content when the probe was asked for it (adoption compares it with the
// plan to avoid a no-op update commit). The lock token lets an adopt-update
// be guarded by optimistic_lock even though there is no prior state for the
// file. dirEntry, set only by probeAdded, names a file or directory inside
// the directory that sits at a path to be created. err is set for any
// failure other than a genuine 404, so callers can tell "absent" from
// "unknown", and every caller fails on it: an adoption built on a failed
// probe would create a file that exists or update one whose content already
// matches (a commit that changes nothing), and a delete skipped because of
// one would let destroy report success while the file still exists.
type remoteProbe struct {
	err             error
	lastCommitID    string
	blobID          string
	dirEntry        string
	content         []byte
	exists          bool
	executeFilemode bool
	hasContent      bool
}

// probeFile reports whether filePath is present at ref and returns its
// metadata, plus the decoded content when withContent is set. Only a genuine
// 404 maps to "absent"; any other failure, an unusable id among them (see
// checkFileIDs), is carried in err.
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
	if err = checkFileIDs(meta); err != nil {
		return remoteProbe{err: err}
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

// checkFileIDs rejects a file response whose blob_id or last_commit_id
// cannot be compared, stored or sent back. client-go builds the metadata
// from response headers and reports a missing one as "", and an id longer
// than maxBlobIDLen is treated as hostile. Each caller decides whether that
// fails the operation or only warns.
func checkFileIDs(f *gitlab.File) error {
	switch {
	case f.BlobID == "" || f.LastCommitID == "":
		return errors.New("GitLab returned no blob_id or last_commit_id")
	case len(f.BlobID) > maxBlobIDLen:
		return fmt.Errorf("GitLab returned a blob_id of unexpected length %d (max %d)", len(f.BlobID), maxBlobIDLen)
	case len(f.LastCommitID) > maxBlobIDLen:
		return fmt.Errorf("GitLab returned a last_commit_id of unexpected length %d (max %d)", len(f.LastCommitID), maxBlobIDLen)
	}
	return nil
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
				return nil, errors.New("the probe of the existing file carried no last_commit_id, so optimistic_lock cannot guard its adoption")
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

// probeEach runs probe for every path, fanned out at refreshParallelism.
// Each path's outcome, a failure included, travels in its remoteProbe for
// the caller to interpret, so one failure never cancels the other probes.
func probeEach(ctx context.Context, paths []string, probe func(context.Context, string) remoteProbe) map[string]remoteProbe {
	out := make(map[string]remoteProbe, len(paths))
	if len(paths) == 0 {
		return out
	}
	probes := make([]remoteProbe, len(paths))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(refreshParallelism)
	for i, p := range paths {
		g.Go(func() error {
			probes[i] = probe(gctx, p)
			return nil
		})
	}
	_ = g.Wait()
	for i, p := range paths {
		out[p] = probes[i]
	}
	return out
}

// probeAdded probes, at ref, the paths a Create or Update adds. With
// adopt_existing each path is probed for adoption, content included
// (probeFile). A path that will be created, being absent or adoption being
// off, is checked for a directory in its place (directoryEntry): Gitaly lets
// a created file replace a directory together with everything in it. That is
// allowed only when the same commit deletes every file in the directory
// first, a directory of managed files turning into a file; deleted holds the
// paths the commit deletes. A failed probe or a directory in the way is an
// error on that file, and nothing is committed. A directory check that
// answered 404 is trusted only once a branch lookup has answered too (see
// treeGitaly404Note). A directory another resource in this run fills after
// the check is caught under the branch lock (see createsOverClaims).
func (r *filesResource) probeAdded(
	ctx context.Context,
	plan filesResourceModel,
	ref string,
	paths []string,
	deleted map[string]bool,
) (map[string]remoteProbe, diag.Diagnostics) {
	project, branch := plan.ProjectID.ValueString(), plan.Branch.ValueString()
	adopt := plan.adoptExisting()
	var treeNotFound atomic.Bool
	probes := probeEach(ctx, paths, func(ctx context.Context, p string) remoteProbe {
		var probe remoteProbe
		if adopt {
			probe = r.probeFile(ctx, project, ref, p, true)
		}
		if probe.err == nil && !probe.exists {
			var notFound bool
			probe.dirEntry, notFound, probe.err = r.directoryEntry(ctx, project, ref, p)
			if notFound {
				treeNotFound.Store(true)
			}
		}
		return probe
	})

	where := fmt.Sprintf("branch %q", branch)
	if ref != branch {
		where = "create_branch_from commit " + ref
	}
	var diags diag.Diagnostics
	for _, p := range paths {
		probe := probes[p]
		at := path.Root("files").AtMapKey(p)
		if probe.err != nil {
			summary, detail := apiErrorDiag(fmt.Sprintf("probing file %q on %s", p, where), project, branch, probe.err)
			diags.AddAttributeError(at, summary, detail+" Nothing was committed.")
			continue
		}
		if probe.dirEntry == "" {
			continue
		}
		kept := probe.dirEntry
		if hasPathUnder(deleted, p) {
			var err error
			if kept, err = r.fileKeptUnder(ctx, project, ref, p, deleted); err != nil {
				summary, detail := apiErrorDiag(fmt.Sprintf("listing directory %q on %s", p, where), project, branch, err)
				diags.AddAttributeError(at, summary, detail+" Nothing was committed.")
				continue
			}
			if kept == "" {
				continue
			}
		}
		diags.AddAttributeError(at, "A directory is in the way",
			fmt.Sprintf("%q is a directory on %s and holds %q. Creating the file would replace the directory and everything "+
				"in it, so nothing was committed. Move the directory's content, or manage the file under another path.", p, where, kept))
	}
	if !diags.HasError() && treeNotFound.Load() {
		if _, err := r.branchExists(ctx, project, branch); err != nil {
			summary, detail := apiErrorDiag("checking the branch after a directory check answered 404", project, branch, err)
			diags.AddError(summary, detail+treeGitaly404Note+" Nothing was committed.")
		}
	}
	return probes, diags
}

// treeGitaly404Note explains a failed branch lookup after a tree listing
// answered 404. The branch lookup reports a Gitaly failure as an error
// whether or not the branch exists, so it vouches for a base commit's
// listing as well.
const treeGitaly404Note = " GitLab answers a tree listing with 404 when Gitaly fails to resolve the ref, exactly as for " +
	"a path that is not a directory, so a 404 is trusted only once the branch lookup succeeds."

// directoryEntry returns the path of an entry in the directory at p on ref,
// or "" when p is not a directory. GitLab answers a tree listing with 404 for
// a path that is missing or holds a file, and also when Gitaly fails to
// resolve ref; notFound reports that answer, for the caller to confirm.
func (r *filesResource) directoryEntry(ctx context.Context, project, ref, p string) (entry string, notFound bool, err error) {
	nodes, _, err := r.client.Repositories.ListTree(project, &gitlab.ListTreeOptions{
		ListOptions: gitlab.ListOptions{PerPage: 1},
		Path:        new(p),
		Ref:         new(ref),
	}, gitlab.WithContext(ctx))
	switch {
	case errors.Is(err, gitlab.ErrNotFound):
		return "", true, nil
	case err != nil:
		return "", false, err
	case len(nodes) == 0:
		return "", false, nil
	case nodes[0] == nil || nodes[0].Path == "":
		return "", false, errors.New("GitLab returned a tree entry without a path")
	}
	return nodes[0].Path, false, nil
}

// fileKeptUnder lists the directory dir on ref recursively and returns the
// first file in it that deleted does not hold, or "" when deleted holds them
// all.
func (r *filesResource) fileKeptUnder(ctx context.Context, project, ref, dir string, deleted map[string]bool) (string, error) {
	opts := &gitlab.ListTreeOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100},
		Path:        new(dir),
		Ref:         new(ref),
		Recursive:   new(true),
	}
	for {
		nodes, resp, err := r.client.Repositories.ListTree(project, opts, gitlab.WithContext(ctx))
		if err != nil {
			return "", err
		}
		for _, n := range nodes {
			if n == nil || n.Path == "" {
				return "", errors.New("GitLab returned a tree entry without a path")
			}
			if n.Type != "tree" && !deleted[n.Path] {
				return n.Path, nil
			}
		}
		if resp.NextPage <= opts.Page {
			return "", nil
		}
		opts.Page = resp.NextPage
	}
}

// hasPathUnder reports whether paths holds a path inside directory dir.
func hasPathUnder(paths map[string]bool, dir string) bool {
	for p := range paths {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

// probeUnguarded probes the delete and chmod actions that go out without a
// last_commit_id (optimistic_lock off, or no token in state), leaving out
// the paths fresh already resolved to a file. Without the token GitLab has
// no guard of its own: it applies the action to whatever tree entry sits at
// the path, so a delete removes a directory that replaced the file and a
// chmod gives a directory a file mode. With unrefreshed (detect_drift false
// in state, so no refresh dropped a file deleted out of band) every delete
// is probed, a locked one included: GitLab rejects the locked delete of a
// missing file with a 400 on every apply, the one that re-enables
// detect_drift included, since a failed apply keeps the stored value. Its
// token is kept, so a file changed out of band still fails the commit. A
// delete whose path no longer holds a file is dropped; a chmod on one
// fails. With the lock on, every token present and a refreshed state
// nothing is probed. branchFound is as absentPaths reports it.
func (r *filesResource) probeUnguarded(
	ctx context.Context,
	project, branch string,
	actions []*gitlab.CommitActionOptions,
	fresh map[string]remoteProbe,
	unrefreshed bool,
	stage string,
) ([]*gitlab.CommitActionOptions, bool, diag.Diagnostics) {
	var paths []string
	for _, a := range actions {
		if fresh[*a.FilePath].exists {
			continue
		}
		unguardedDelete := *a.Action == gitlab.FileDelete && (a.LastCommitID == nil || unrefreshed)
		unguardedChmod := *a.Action == gitlab.FileChmod && a.LastCommitID == nil
		if unguardedDelete || unguardedChmod {
			paths = append(paths, *a.FilePath)
		}
	}
	if len(paths) == 0 {
		return actions, true, nil
	}
	absent, branchFound, diags := r.absentPaths(ctx, project, branch, paths, stage)
	if diags.HasError() {
		return nil, branchFound, diags
	}
	next := "Refresh the state with detect_drift enabled and review the plan."
	if unrefreshed {
		next = "detect_drift is false in this resource's state, so a refresh keeps the file there: " + recordDetectDrift +
			replanWithChange + "."
	}
	for _, a := range actions {
		if absent[*a.FilePath] && *a.Action == gitlab.FileChmod {
			diags.AddAttributeError(path.Root("files").AtMapKey(*a.FilePath), "File no longer exists",
				fmt.Sprintf("cannot change execute_filemode of %q: the path no longer holds a file on branch %q "+
					"(deleted, or replaced by a directory, out of band). %s", *a.FilePath, branch, next))
		}
	}
	if diags.HasError() {
		return nil, branchFound, diags
	}
	return withoutPaths(actions, absent), branchFound, diags
}

// gitaly404Note explains a failed branch lookup after a Files API 404.
const gitaly404Note = " GitLab answers the Files API with 404 when Gitaly times out resolving the ref " +
	"(and, up to at least 19.4, when Gitaly is unavailable), exactly as for a missing file, " +
	"so a 404 is trusted only once the branch lookup succeeds."

// absentPaths probes paths at branch and returns the ones that are gone.
// A 404 is trusted only once a branch lookup has answered too: GitLab
// answers the Files API with 404 when Gitaly fails to resolve the ref (a
// timeout in every release, an outage too up to at least 19.4), while the
// branches API reports the same failure as an error. Any other probe
// failure is an error as well, since skipping a path whose state is unknown
// would orphan the file. branchFound is false only when a path was gone and
// the branch was not found either.
func (r *filesResource) absentPaths(ctx context.Context, project, branch string, paths []string, stage string) (map[string]bool, bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	probes := probeEach(ctx, paths, func(ctx context.Context, p string) remoteProbe {
		return r.probeFile(ctx, project, branch, p, false)
	})
	absent := map[string]bool{}
	for _, p := range paths {
		switch probe := probes[p]; {
		case probe.err != nil:
			summary, detail := apiErrorDiag(fmt.Sprintf("probing file %q %s", p, stage), project, branch, probe.err)
			diags.AddError(summary, detail)
		case !probe.exists:
			absent[p] = true
		}
	}
	if diags.HasError() || len(absent) == 0 {
		return nil, true, diags
	}
	found, err := r.branchExists(ctx, project, branch)
	if err != nil {
		summary, detail := apiErrorDiag("checking the branch after a managed file answered 404", project, branch, err)
		diags.AddError(summary, detail+gitaly404Note+" Nothing was committed.")
		return nil, false, diags
	}
	return absent, found, diags
}

// withoutPaths returns the actions whose path is not in drop.
func withoutPaths(actions []*gitlab.CommitActionOptions, drop map[string]bool) []*gitlab.CommitActionOptions {
	kept := make([]*gitlab.CommitActionOptions, 0, len(actions))
	for _, a := range actions {
		if !drop[*a.FilePath] {
			kept = append(kept, a)
		}
	}
	return kept
}

// errLockWait marks a commit that was never sent: ctx ended while it waited
// for the branch lock.
var errLockWait = errors.New("cancelled while waiting for the branch lock")

// commitLocked lands actions as one commit while holding the branch lock, so
// resource instances sharing a branch never race on its tip, and releases
// the lock before it returns: whatever follows only reads. Under the lock it
// first leaves out the deletes of paths another resource in this process
// has claimed (withoutClaimedDeletes); outside it, a resource could claim
// and probe a path between that check and the commit. sent is what went
// out, and when it is empty nothing was sent. diags carries a warning per
// delete left out, or the error that stopped the commit before it was sent.
func (r *filesResource) commitLocked(
	ctx context.Context,
	m filesResourceModel,
	actions []*gitlab.CommitActionOptions,
) (commit *gitlab.Commit, sent []*gitlab.CommitActionOptions, diags diag.Diagnostics, err error) {
	project, branch := m.ProjectID.ValueString(), m.Branch.ValueString()
	release, err := r.locks.acquire(ctx, project, branch)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %w", errLockWait, err)
	}
	defer release()
	sent, diags = r.withoutClaimedDeletes(ctx, project, branch, actions)
	if diags.HasError() || len(sent) == 0 {
		return nil, nil, diags, nil
	}
	commit, _, err = r.client.Commits.CreateCommit(project, commitOptions(m, sent), r.commitRequestOptions(ctx)...)
	return commit, sent, diags, err
}

// withoutClaimedDeletes returns actions without the deletes of paths that
// another resource in this process has claimed on branch, with a warning for
// each. A resource never blocks its own deletes: it claims only paths it
// adds, and those are never among the paths it deletes in the same
// operation. The creates left are then checked against the claims too (see
// createsOverClaims), a delete left out inside a directory the commit turns
// into a file among them.
func (r *filesResource) withoutClaimedDeletes(
	ctx context.Context,
	project, branch string,
	actions []*gitlab.CommitActionOptions,
) ([]*gitlab.CommitActionOptions, diag.Diagnostics) {
	var diags diag.Diagnostics
	kept := make([]*gitlab.CommitActionOptions, 0, len(actions))
	for _, a := range actions {
		if *a.Action != gitlab.FileDelete {
			kept = append(kept, a)
			continue
		}
		p := *a.FilePath
		claimedAs, err := r.claimedUnder(ctx, project, branch, p)
		switch {
		case err != nil:
			diags.AddError("Cannot tell whether another resource owns a file",
				fmt.Sprintf("%q on branch %q is to be deleted through project_id %q, and another gitlabcommits_files "+
					"resource in this run adopted or wrote that path through project_id %q. Whether the two name the same "+
					"project is unknown: looking them up failed (%s), so nothing was committed. If they name different "+
					"projects, apply again. If they name the same project, do not simply apply again: a later run cannot "+
					"see this run's claim and would delete the file the other resource now manages. Remove this resource "+
					"from state instead (terraform state rm <address>, which also drops a replaced object still waiting "+
					"for its destroy and leaves every file in place), import it again with terraform import if the "+
					"configuration still declares it, and then apply.",
					p, branch, project, claimedAs, err))
			return nil, diags
		case claimedAs == "":
			kept = append(kept, a)
		case claimedAs == project:
			diags.AddWarning("File left in place for another resource",
				fmt.Sprintf("another gitlabcommits_files resource in this run adopted or wrote %q on branch %q of project %q, "+
					"so it was not deleted; it now belongs to that resource.", p, branch, project))
		default:
			diags.AddWarning("File left in place for another resource",
				fmt.Sprintf("another gitlabcommits_files resource in this run adopted or wrote %q on branch %q through "+
					"project_id %q, the same project as %q, so it was not deleted; it now belongs to that resource.",
					p, branch, claimedAs, project))
		}
	}
	diags.Append(r.createsOverClaims(ctx, project, branch, kept)...)
	if diags.HasError() {
		return nil, diags
	}
	return kept, diags
}

// createsOverClaims fails a commit that creates a file where a resource in
// this process claimed a path inside a directory of that name on branch:
// Gitaly would replace the directory, and the claimed file with it. The
// directory check in probeAdded cannot see a file another resource commits
// after it ran, but that resource claimed the path before its commit, so
// checking the claims under the branch lock, right before the commit,
// leaves no gap. A claim of a failed Create that never wrote its path fails
// the commit as well, which is the safe direction.
func (r *filesResource) createsOverClaims(
	ctx context.Context,
	project, branch string,
	actions []*gitlab.CommitActionOptions,
) diag.Diagnostics {
	created := map[string]bool{}
	for _, a := range actions {
		if *a.Action == gitlab.FileCreate {
			created[*a.FilePath] = true
		}
	}
	if len(created) == 0 {
		return nil
	}
	var diags diag.Diagnostics
	inside := r.locks.claimedInside(branch, created)
	for _, p := range sortedKeys(inside) {
		dir := inside[p]
		at := path.Root("files").AtMapKey(dir)
		claimedAs, err := r.claimedUnder(ctx, project, branch, p)
		switch {
		case err != nil:
			diags.AddAttributeError(at, "Cannot tell whether another resource owns a file",
				fmt.Sprintf("%q is to be created on branch %q through project_id %q, which would replace the directory holding "+
					"%q, and a gitlabcommits_files resource in this run adopts or writes that path through project_id %q. "+
					"Whether the two name the same project is unknown: looking them up failed (%s), so nothing was committed. "+
					"Apply again once the lookup succeeds.", dir, branch, project, p, claimedAs, err))
			return diags
		case claimedAs == "":
			continue
		}
		through := ""
		if claimedAs != project {
			through = fmt.Sprintf(" through project_id %q, the same project as %q", claimedAs, project)
		}
		diags.AddAttributeError(at, "A file another resource manages is in the way",
			fmt.Sprintf("%q cannot be created on branch %q: it would replace the directory holding %q, which a "+
				"gitlabcommits_files resource in this run adopts or writes%s, so nothing was committed. Manage the file "+
				"under another path.", dir, branch, p, through))
		return diags
	}
	return nil
}

// claimedUnder returns the project_id spelling under which a resource in
// this process claimed filePath on branch of project, or "" when none did. A
// claim under another spelling counts when both name the same project; that
// takes one GetProject per path spelling, made only then and cached for the
// process. A failed lookup is an error, returned with the spelling it was
// comparing against, since neither deleting the file nor keeping it would be
// known to be right.
func (r *filesResource) claimedUnder(ctx context.Context, project, branch, filePath string) (string, error) {
	claimants := r.locks.claimants(branch, filePath)
	if slices.Contains(claimants, project) {
		return project, nil
	}
	for _, other := range claimants {
		same, err := r.sameProject(ctx, project, other)
		if err != nil {
			return other, err
		}
		if same {
			return other, nil
		}
	}
	return "", nil
}

// sameProject reports whether two project_id spellings name one project, by
// the numeric ID GitLab returns for each.
func (r *filesResource) sameProject(ctx context.Context, a, b string) (bool, error) {
	idA, err := r.projectNumericID(ctx, a)
	if err != nil {
		return false, err
	}
	idB, err := r.projectNumericID(ctx, b)
	if err != nil {
		return false, err
	}
	return idA == idB, nil
}

// projectNumericID resolves a project_id spelling to the project's numeric
// ID, once per process. An all-digit spelling is that ID already: GitLab
// resolves an all-digit project_id by ID (a project path always holds a
// slash), so it needs no request, and "0123" is project 123.
func (r *filesResource) projectNumericID(ctx context.Context, project string) (int64, error) {
	if id, ok := allDigitProjectID(project); ok {
		return id, nil
	}
	if id, ok := r.locks.projectID(project); ok {
		return id, nil
	}
	proj, _, err := r.client.Projects.GetProject(project, nil, gitlab.WithContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("looking up project %q: %w", project, err)
	}
	if proj == nil || proj.ID == 0 {
		return 0, fmt.Errorf("looking up project %q: GitLab returned no project id", project)
	}
	r.locks.setProjectID(project, proj.ID)
	return proj.ID, nil
}

// allDigitProjectID parses a project_id made of ASCII digits only.
func allDigitProjectID(project string) (int64, bool) {
	if project == "" || strings.ContainsFunc(project, func(c rune) bool { return c < '0' || c > '9' }) {
		return 0, false
	}
	id, err := strconv.ParseInt(project, 10, 64)
	return id, err == nil
}

// commitErrorDiag is apiErrorDiag for an error from commitLocked.
func commitErrorDiag(action, project, branch string, err error) (string, string) {
	if errors.Is(err, errLockWait) {
		return "Cancelled while waiting for the branch lock", err.Error()
	}
	return apiErrorDiag(action, project, branch, err)
}

// recordDetectDrift is the way back to refreshes while state records
// detect_drift = false: a refresh reads the stored value, not the
// configuration, and a failed apply keeps it, so the new value has to be
// recorded by an apply that changes nothing else.
const recordDetectDrift = "set detect_drift = true with `files` as last applied and apply (this makes no commit)"

// replanWithChange follows recordDetectDrift when an apply was making a
// change. The plan after the recording apply compares files with the
// branch, so with files still as last applied it would revert whatever
// changed there, the change itself included when its commit had landed.
const replanWithChange = ", then put your change back in `files` and plan again"

// unrefreshedNote is the advice to append to a commit error of Update, or of
// Delete with destroy set, when state records detect_drift = false: a
// refresh then leaves state as it is, so neither the refresh a lock conflict
// suggests nor the plan a server error suggests can help. It is "" for any
// other error, and when state records detect_drift = true. For Update the
// lock can only be dropped as a shortcut: a token-less update of a file
// deleted out of band still fails, and after a server error whose commit
// landed it would commit the same change a second time.
func unrefreshedNote(state filesResourceModel, err error, destroy bool) string {
	if state.detectDrift() {
		return ""
	}
	const lead = " detect_drift is false in this resource's state, so "
	switch {
	case isLockConflict(err) && destroy:
		return lead + "the refresh before destroy leaves the stored last_commit_id values as they are and destroying " +
			"again fails the same way. To delete the files whatever the other edit changed, set optimistic_lock = false " +
			"with `files` as last applied and apply (this makes no commit), then destroy again; to review the edit " +
			"first, " + recordDetectDrift + ", then plan again."
	case isLockConflict(err):
		return lead + "a refresh leaves the stored last_commit_id values as they are and applying again fails the same " +
			"way. To see the other edit, " + recordDetectDrift + replanWithChange + ": the refresh then reads the " +
			"branch, and the plan shows your change against what is there. Applying once with optimistic_lock = false " +
			"(then setting it back) is only a shortcut for overwriting an edit to a file that still exists: it does not " +
			"help when a file was deleted out of band, and after a failed apply whose commit may have landed it would " +
			"commit the same change a second time."
	case isServerError(err) && destroy:
		return lead + "terraform plan cannot show whether the destroy commit landed. Running terraform destroy again " +
			"is safe: the files that commit already deleted are skipped."
	case isServerError(err):
		return lead + "terraform plan cannot show whether the commit landed. To find out, " + recordDetectDrift +
			replanWithChange + ": the refresh then reads the branch, and a change that already landed needs no commit."
	}
	return ""
}

// hasStatus reports whether err is a GitLab answer with the given HTTP status.
func hasStatus(err error, status int) bool {
	resp, ok := errors.AsType[*gitlab.ErrorResponse](err)
	return ok && resp.Response != nil && resp.Response.StatusCode == status
}

// isServerError reports whether err is a GitLab answer with a 5xx status.
func isServerError(err error) bool {
	resp, ok := errors.AsType[*gitlab.ErrorResponse](err)
	return ok && resp.Response != nil && resp.Response.StatusCode >= 500
}

// isLockConflict reports whether err is GitLab's optimistic-lock rejection:
// a 400 or 409 saying a file changed since the last_commit_id sent. The
// commits API (Files::MultiService) answers "The file has changed since you
// started editing it: <path>"; the single-file Files API and the web editor
// use "You are attempting to update a file that has changed since you
// started editing it." Three substrings are matched - `last_commit_id`
// (snake_case, future-proof if the API ever exposes the parameter name),
// `last commit` (current prose form), and `has changed since` (most stable
// phrase) - so any one surviving a future rewording keeps it recognised.
func isLockConflict(err error) bool {
	resp, ok := errors.AsType[*gitlab.ErrorResponse](err)
	if !ok || resp.Response == nil {
		return false
	}
	if s := resp.Response.StatusCode; s != http.StatusBadRequest && s != http.StatusConflict {
		return false
	}
	lower := strings.ToLower(resp.Message)
	return strings.Contains(lower, "last_commit_id") ||
		strings.Contains(lower, "last commit") ||
		strings.Contains(lower, "has changed since")
}

// tagShadowHint names the other cause of a rejection that reads like
// another writer's: a tag with the branch's name. GitLab resolves the bare
// name to the tag first, and its commits API checks a commit against the
// branch by that bare name; when names that check and what then fails.
func tagShadowHint(branch, when string) string {
	return fmt.Sprintf("check whether a tag named %q exists (`git ls-remote <remote> refs/tags/%s`, or the project's "+
		"Code > Tags page): GitLab resolves the bare branch name to that tag when it %s until the tag is renamed or "+
		"deleted. A branch that shares its name with a tag is not supported.", branch, branch, when)
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

// missingBranchPreflight checks that an absent branch can be materialised
// and returns the commit it starts from: create_branch_from itself when that
// is a commit SHA, lowercased because Gitaly takes only lowercase hex as
// start_sha, otherwise the head of that branch. Resolving it once keeps the
// adopt probes and the new branch on one tree while the branch moves. A 404
// on the project means the project itself is the problem (missing, or
// invisible to the token), not the branch. A repository with no commits has
// no ref to start from: without create_branch_from the first commit becomes
// its root commit and creates the branch, so the commit returned is "".
func (r *filesResource) missingBranchPreflight(ctx context.Context, project, branch, createFrom string) (string, error) {
	proj, _, err := r.client.Projects.GetProject(project, nil, gitlab.WithContext(ctx))
	if err != nil {
		if errors.Is(err, gitlab.ErrNotFound) {
			return "", fmt.Errorf("project %q does not exist or the token cannot see it (GitLab answers 404 for both); "+
				"check project_id and the token's scope and membership", project)
		}
		return "", fmt.Errorf("checking project %q: %w", project, err)
	}
	if proj != nil && proj.EmptyRepo {
		if createFrom != "" {
			return "", fmt.Errorf("repository %q has no commits, so create_branch_from %q has nothing to start from; "+
				"remove create_branch_from and the first commit creates branch %q in the empty repository", project, createFrom, branch)
		}
		return "", nil
	}
	if createFrom == "" {
		return "", fmt.Errorf("branch %q does not exist; set create_branch_from to materialise it", branch)
	}
	if isCommitSHA(createFrom) {
		return strings.ToLower(createFrom), nil
	}
	src, _, err := r.client.Branches.GetBranch(project, createFrom, gitlab.WithContext(ctx))
	if err != nil {
		if errors.Is(err, gitlab.ErrNotFound) {
			return "", fmt.Errorf("create_branch_from branch %q does not exist in project %q; it takes a branch name "+
				"or a full commit SHA (tags are not supported)", createFrom, project)
		}
		return "", fmt.Errorf("resolving create_branch_from branch %q: %w", createFrom, err)
	}
	if src == nil || src.Commit == nil || src.Commit.ID == "" {
		return "", fmt.Errorf("resolving create_branch_from branch %q: GitLab returned no head commit", createFrom)
	}
	return src.Commit.ID, nil
}

// describeBase names the commit a missing branch is created from, as the
// configuration gave it, for diagnostics.
func describeBase(createFrom, base string) string {
	if isCommitSHA(createFrom) {
		return fmt.Sprintf("create_branch_from commit %s", base)
	}
	return fmt.Sprintf("create_branch_from ref %q at commit %s", createFrom, base)
}

// createBranch materialises branch at commit base without a commit. Only
// used when adoption found nothing to commit; otherwise start_sha on the
// first commit does both in one operation.
func (r *filesResource) createBranch(ctx context.Context, project, branch, base string) error {
	_, _, err := r.client.Branches.CreateBranch(project, &gitlab.CreateBranchOptions{
		Branch: new(branch),
		Ref:    new(base),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("creating branch %q from commit %s: %w", branch, base, err)
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
// CreateCommit touched. BlobID is pulled via parallel GetFileMetaData probes
// (HEAD-style) so state reflects what GitLab returns, including any future
// blob-id format (SHA-256 repos). Paths the commit did not touch are not
// probed at all; the caller carries their values over from state
// (carryOver), so a probe hiccup can never stamp this commit's SHA next to a
// file it never modified. A nil touched set stamps nothing.
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
// Fail-soft covers a probe error and a 2xx whose ids checkFileIDs rejects
// (missing, or longer than maxBlobIDLen). In both BlobID is left null,
// LastCommitID keeps the commitSHA first-pass stamp (correct for a touched
// file), and a warning is appended; the next Read repopulates both.
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
		if touched[p] {
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
			if err := checkFileIDs(meta); err != nil {
				results[i].err = err
				return nil //nolint:nilerr // intentional: an unusable id is a failed probe, not a value to store
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

// maxBlobIDLen bounds a server-returned blob_id or last_commit_id we will
// store in state or send back as a lock token. Both are git SHAs (40 hex
// today, 64 on SHA-256 repos); anything past this generous ceiling (well
// above SHA-512 hex) is unexpected and treated as hostile.
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
	return s[:cut] + fmt.Sprintf("... (truncated, %d more chars)", len(s)-cut)
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

	// ErrNotFound is itself an *ErrorResponse (StatusCode 404, nil Response)
	// and (*ErrorResponse).Is matches on the status code alone, so errors.Is
	// catches every 404 client-go can produce, shared sentinel or not. It has
	// to: the sentinel carries no *http.Response, so the guarded switch below
	// skips it and the 404 would reach the bare err.Error() tail. The order
	// earns its keep as well, since a 404 that did arrive with a Response
	// would otherwise land in that switch's default.
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
			return summary, fmt.Sprintf("%s: token rejected. Verify the token has the `api` scope (or, for a fine-grained token, the required permissions) and is not expired. Body: %s", prefix, body)
		case 403:
			summary = "GitLab permission denied (HTTP 403)"
			return summary, fmt.Sprintf("%s: %s Body: %s", prefix,
				"The token was rejected by GitLab. Verify that: "+
					"(1) the token has the `api` scope, or the fine-grained permissions Commit: Create, Repository: Read and Branch: Read (write_repository alone does not authenticate REST API calls); "+
					"(2) the token's user has the Developer role on the project, or Maintainer for a protected branch; "+
					"(3) if you are using CI_JOB_TOKEN, switch to a Personal / Project / Group access token - job tokens cannot POST to /repository/commits; "+
					"(4) if your group or instance enforces fine-grained personal access tokens, a legacy `api` token is refused after the enforcement date and the body lists the permissions a fine-grained token needs.",
				body)
		case 400, 409:
			if isLockConflict(err) {
				summary = "Concurrent modification detected (optimistic_lock)"
				return summary, fmt.Sprintf("%s: a file was modified by someone else since this resource last touched it. "+
					"Run `terraform apply -refresh-only` to pull current state, then re-plan. If nobody else touched it, %s "+
					"Body: %s", prefix, tagShadowHint(branch, "checks last_commit_id, so the check fails"), body)
			}
			// Gitaly refuses to move the ref when the branch tip changed between
			// reading it and writing the commit: "reference update: reference
			// does not point to expected object". Commits are serialised per
			// branch inside this process, so this means a writer outside this
			// terraform run, or a tag of the same name: GitLab takes the tip it
			// expects from the bare branch name, which resolves to the tag.
			if strings.Contains(strings.ToLower(resp.Message), "expected object") {
				summary = "Branch changed while the commit was being created"
				return summary, fmt.Sprintf("%s: another writer pushed to the branch while GitLab was building this commit, "+
					"so the ref update was refused and nothing was committed. This provider serialises its own commits per "+
					"branch within one provider configuration (each provider block runs in its own process), so the other "+
					"writer is another process: a different pipeline, a manual push, a bot, or a second provider block "+
					"(alias) targeting the same branch. Wait for it to finish and re-run terraform apply. If this happens "+
					"on every run with no other writer, %s Body: %s", prefix,
					tagShadowHint(branch, "computes the branch tip it expects, so every commit to the branch is refused"), body)
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
// passing through whatever string came on the wire - a text file whose
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
