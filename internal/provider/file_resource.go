package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// Global variables for batch manager singleton.
var (
	// batchManager is the singleton instance of CommitBatchManager.
	batchManager *CommitBatchManager
	// batchManagerOnce ensures the batch manager is initialized only once.
	batchManagerOnce sync.Once
)

// CommitBatchManager manages batching of file operations into single commits.
// It collects file operations from multiple resources and groups them into
// a single commit after a timeout period.
type CommitBatchManager struct {
	// mu protects concurrent access to the batches map.
	mu sync.Mutex
	// batches stores active commit batches keyed by "projectID/branch".
	batches map[string]*CommitBatch
}

// CommitBatch represents a batch of file operations for a single commit.
// Multiple file resources can add their operations to the same batch,
// which will be committed together after the timeout expires.
type CommitBatch struct {
	// ProjectID is the GitLab project identifier.
	ProjectID string
	// Branch is the target branch name.
	Branch string
	// CommitMessage is the message for the batched commit.
	CommitMessage string
	// AuthorEmail is the commit author's email (optional).
	AuthorEmail string
	// AuthorName is the commit author's name (optional).
	AuthorName string
	// Files contains file operations keyed by file path.
	Files map[string]*gitlab.CommitActionOptions
	// ReadyChan signals when the batch timeout has expired.
	ReadyChan chan struct{}
	// ResultChan delivers the commit result to waiting resources.
	ResultChan chan *CommitResult
	// Timeout is the duration to wait before processing the batch.
	Timeout time.Duration
}

// CommitResult holds the result of a commit operation.
type CommitResult struct {
	// CommitSHA is the SHA of the created commit.
	CommitSHA string
	// Error contains any error that occurred during commit creation.
	Error error
}

// getBatchManager returns the singleton batch manager instance.
// It lazily initializes the manager on first access using sync.Once.
func getBatchManager() *CommitBatchManager {
	batchManagerOnce.Do(func() {
		batchManager = &CommitBatchManager{
			batches: make(map[string]*CommitBatch),
		}
	})
	return batchManager
}

// getBatchKey generates a unique key for a batch based on project and branch.
// This ensures files for the same project/branch are grouped together.
func (m *CommitBatchManager) getBatchKey(projectID, branch string) string {
	return fmt.Sprintf("%s/%s", projectID, branch)
}

// getOrCreateBatch retrieves an existing batch or creates a new one.
// If a new batch is created, it starts a timeout timer that will trigger
// batch execution after the timeout period. Thread-safe.
func (m *CommitBatchManager) getOrCreateBatch(projectID, branch, commitMessage, authorEmail, authorName string) *CommitBatch {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := m.getBatchKey(projectID, branch)
	batch, exists := m.batches[key]
	if !exists {
		batch = &CommitBatch{
			ProjectID:     projectID,
			Branch:        branch,
			CommitMessage: commitMessage,
			AuthorEmail:   authorEmail,
			AuthorName:    authorName,
			Files:         make(map[string]*gitlab.CommitActionOptions),
			ReadyChan:     make(chan struct{}),
			ResultChan:    make(chan *CommitResult, 1),
			Timeout:       5 * time.Second, // Collect files for 5 seconds
		}
		m.batches[key] = batch

		// Start timeout timer in a separate goroutine.
		// This will execute the batch once after the timeout expires.
		go m.processBatchAfterTimeout(batch)
	}
	return batch
}

// processBatchAfterTimeout waits for the batch timeout and signals readiness.
// This runs in a separate goroutine and closes the ReadyChan when the timeout
// expires, triggering the batch execution in executeBatch.
func (m *CommitBatchManager) processBatchAfterTimeout(batch *CommitBatch) {
	timer := time.NewTimer(batch.Timeout)
	defer timer.Stop()

	<-timer.C
	// Timeout reached, signal that batch is ready to process.
	close(batch.ReadyChan)
}

// addFileToBatch adds a file operation to an existing batch. Thread-safe.
func (m *CommitBatchManager) addFileToBatch(batch *CommitBatch, filePath string, action *gitlab.CommitActionOptions) {
	m.mu.Lock()
	defer m.mu.Unlock()
	batch.Files[filePath] = action
}

// executeBatch processes a batch by creating a single commit with all files.
// This method blocks until the batch timeout expires (via ReadyChan), then
// creates a commit with all collected file operations and sends the result
// to all waiting resources via ResultChan.
func (m *CommitBatchManager) executeBatch(client *gitlab.Client, batch *CommitBatch) {
	// Wait for batch to be ready (timeout expired).
	<-batch.ReadyChan

	m.mu.Lock()
	// Build actions array from collected files.
	actions := make([]*gitlab.CommitActionOptions, 0, len(batch.Files))
	for _, action := range batch.Files {
		actions = append(actions, action)
	}
	m.mu.Unlock()

	var result *CommitResult

	if len(actions) == 0 {
		result = &CommitResult{Error: fmt.Errorf("no files to commit")}
	} else {
		// Create commit options
		commitOpts := &gitlab.CreateCommitOptions{
			Branch:        gitlab.Ptr(batch.Branch),
			CommitMessage: gitlab.Ptr(batch.CommitMessage),
			Actions:       actions,
		}

		if batch.AuthorEmail != "" {
			commitOpts.AuthorEmail = gitlab.Ptr(batch.AuthorEmail)
		}

		if batch.AuthorName != "" {
			commitOpts.AuthorName = gitlab.Ptr(batch.AuthorName)
		}

		// Create commit
		commit, _, err := client.Commits.CreateCommit(batch.ProjectID, commitOpts)
		if err != nil {
			result = &CommitResult{Error: err}
		} else {
			result = &CommitResult{CommitSHA: commit.ID}
		}
	}

	// Send result to all waiting resources (buffered channel).
	batch.ResultChan <- result
}

// clearBatch removes a batch from the manager after it has been processed.
// Thread-safe.
func (m *CommitBatchManager) clearBatch(projectID, branch string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.getBatchKey(projectID, branch)
	delete(m.batches, key)
}

// Ensure the implementation satisfies the expected interfaces.
var (
	_ resource.Resource                = &fileResource{}
	_ resource.ResourceWithConfigure   = &fileResource{}
	_ resource.ResourceWithImportState = &fileResource{}
)

// NewFileResource is a helper function to simplify the provider implementation.
func NewFileResource() resource.Resource {
	return &fileResource{}
}

// fileResource is the resource implementation.
type fileResource struct {
	client *gitlab.Client
}

// fileResourceModel maps the resource schema data.
type fileResourceModel struct {
	ID            types.String `tfsdk:"id"`
	ProjectID     types.String `tfsdk:"project_id"`
	Branch        types.String `tfsdk:"branch"`
	FilePath      types.String `tfsdk:"file_path"`
	Content       types.String `tfsdk:"content"`
	ContentBase64 types.String `tfsdk:"content_base64"`
	Action        types.String `tfsdk:"action"`
	Encoding      types.String `tfsdk:"encoding"`
	CommitMessage types.String `tfsdk:"commit_message"`
	AuthorEmail   types.String `tfsdk:"author_email"`
	AuthorName    types.String `tfsdk:"author_name"`
	CommitSHA     types.String `tfsdk:"commit_sha"`
	BatchMode     types.Bool   `tfsdk:"batch_mode"`
}

// Metadata returns the resource type name.
func (r *fileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file"
}

// Schema defines the schema for the resource.
func (r *fileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a single file in a GitLab repository. When batch_mode is enabled and multiple files share the same project/branch/commit_message, they are automatically batched into a single commit.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "Internal identifier for the resource.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": schema.StringAttribute{
				Description: "The ID or URL-encoded path of the project.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"branch": schema.StringAttribute{
				Description: "Name of the branch to commit to.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"file_path": schema.StringAttribute{
				Description: "Full path to the file in the repository.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"content": schema.StringAttribute{
				Description: "File content. Mutually exclusive with content_base64.",
				Optional:    true,
			},
			"content_base64": schema.StringAttribute{
				Description: "Base64 encoded file content. Mutually exclusive with content.",
				Optional:    true,
			},
			"action": schema.StringAttribute{
				Description: "Action to perform: create, delete, move, update, chmod. Default is update.",
				Optional:    true,
			},
			"encoding": schema.StringAttribute{
				Description: "Encoding of the file content: text or base64. Default is text.",
				Optional:    true,
			},
			"commit_message": schema.StringAttribute{
				Description: "Commit message. When batch_mode is enabled, files with same commit_message are batched together.",
				Required:    true,
			},
			"author_email": schema.StringAttribute{
				Description: "Author email address.",
				Optional:    true,
			},
			"author_name": schema.StringAttribute{
				Description: "Author name.",
				Optional:    true,
			},
			"commit_sha": schema.StringAttribute{
				Description: "The SHA of the created commit.",
				Computed:    true,
			},
			"batch_mode": schema.BoolAttribute{
				Description: "Enable batching of multiple file operations into a single commit. Default is true.",
				Optional:    true,
			},
		},
	}
}

// Configure adds the provider configured client to the resource.
func (r *fileResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*gitlab.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *gitlab.Client, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)
		return
	}

	r.client = client
}

// Create creates the resource and sets the initial Terraform state.
func (r *fileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileResourceModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Build commit action
	action, err := r.buildCommitAction(&plan)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error Building Commit Action",
			"Could not build commit action: "+err.Error(),
		)
		return
	}

	// Check batch mode (default is true)
	batchMode := true
	if !plan.BatchMode.IsNull() {
		batchMode = plan.BatchMode.ValueBool()
	}

	var commitSHA string
	if batchMode {
		// Use batch manager
		manager := getBatchManager()
		batch := manager.getOrCreateBatch(
			plan.ProjectID.ValueString(),
			plan.Branch.ValueString(),
			plan.CommitMessage.ValueString(),
			plan.AuthorEmail.ValueString(),
			plan.AuthorName.ValueString(),
		)

		// Check if this is the first file (need to start batch executor)
		manager.mu.Lock()
		isFirstFile := len(batch.Files) == 0
		manager.mu.Unlock()

		// Add file to batch
		manager.addFileToBatch(batch, plan.FilePath.ValueString(), action)

		// If this is the first file, start the batch executor goroutine
		if isFirstFile {
			go func() {
				manager.executeBatch(r.client, batch)
				manager.clearBatch(plan.ProjectID.ValueString(), plan.Branch.ValueString())
			}()
		}

		// Wait for batch result (all files get the same result)
		result := <-batch.ResultChan
		// Put result back for other waiting resources
		batch.ResultChan <- result

		if result.Error != nil {
			resp.Diagnostics.AddError(
				"Error Creating Commit",
				"Could not create batched commit: "+result.Error.Error(),
			)
			return
		}

		commitSHA = result.CommitSHA
	} else {
		// Direct commit (single file)
		commitOpts := &gitlab.CreateCommitOptions{
			Branch:        gitlab.Ptr(plan.Branch.ValueString()),
			CommitMessage: gitlab.Ptr(plan.CommitMessage.ValueString()),
			Actions:       []*gitlab.CommitActionOptions{action},
		}

		if !plan.AuthorEmail.IsNull() {
			commitOpts.AuthorEmail = gitlab.Ptr(plan.AuthorEmail.ValueString())
		}

		if !plan.AuthorName.IsNull() {
			commitOpts.AuthorName = gitlab.Ptr(plan.AuthorName.ValueString())
		}

		commit, _, err := r.client.Commits.CreateCommit(plan.ProjectID.ValueString(), commitOpts)
		if err != nil {
			resp.Diagnostics.AddError(
				"Error Creating Commit",
				"Could not create commit: "+err.Error(),
			)
			return
		}

		commitSHA = commit.ID
	}

	// Set state
	plan.ID = types.StringValue(r.generateID(plan.ProjectID.ValueString(), plan.Branch.ValueString(), plan.FilePath.ValueString()))
	plan.CommitSHA = types.StringValue(commitSHA)

	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
}

// Read refreshes the Terraform state with the latest data.
func (r *fileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state fileResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Try to read file from repository
	file, _, err := r.client.RepositoryFiles.GetFile(
		state.ProjectID.ValueString(),
		state.FilePath.ValueString(),
		&gitlab.GetFileOptions{
			Ref: gitlab.Ptr(state.Branch.ValueString()),
		},
	)

	if err != nil {
		// File doesn't exist - remove from state
		resp.State.RemoveResource(ctx)
		return
	}

	// Update content in state
	if file.Content != "" {
		content, err := base64.StdEncoding.DecodeString(file.Content)
		if err == nil {
			state.Content = types.StringValue(string(content))
		}
	}

	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
}

// Update updates the resource and sets the updated Terraform state on success.
func (r *fileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// For updates, we use the same logic as Create
	var plan fileResourceModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Build commit action
	action, err := r.buildCommitAction(&plan)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error Building Commit Action",
			"Could not build commit action: "+err.Error(),
		)
		return
	}

	// Always use direct commit for updates (no batching)
	commitOpts := &gitlab.CreateCommitOptions{
		Branch:        gitlab.Ptr(plan.Branch.ValueString()),
		CommitMessage: gitlab.Ptr(plan.CommitMessage.ValueString()),
		Actions:       []*gitlab.CommitActionOptions{action},
	}

	if !plan.AuthorEmail.IsNull() {
		commitOpts.AuthorEmail = gitlab.Ptr(plan.AuthorEmail.ValueString())
	}

	if !plan.AuthorName.IsNull() {
		commitOpts.AuthorName = gitlab.Ptr(plan.AuthorName.ValueString())
	}

	commit, _, err := r.client.Commits.CreateCommit(plan.ProjectID.ValueString(), commitOpts)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error Updating File",
			"Could not create commit: "+err.Error(),
		)
		return
	}

	plan.CommitSHA = types.StringValue(commit.ID)

	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
}

// Delete deletes the resource and removes the Terraform state on success.
func (r *fileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state fileResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Note: We don't actually delete the file from GitLab
	// The resource is just removed from Terraform state
	// If you want to delete the file, use action="delete" before destroying
}

// ImportState imports the resource state.
func (r *fileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// Helper methods

// generateID creates a unique identifier for the file resource.
// It uses SHA256 hash of project/branch/filepath to ensure uniqueness.
func (r *fileResource) generateID(projectID, branch, filePath string) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s", projectID, branch, filePath)))
	return fmt.Sprintf("%x", hash)
}

// buildCommitAction converts a file resource model to a GitLab commit action.
// It handles content encoding, action type validation, and content validation.
func (r *fileResource) buildCommitAction(model *fileResourceModel) (*gitlab.CommitActionOptions, error) {
	action := &gitlab.CommitActionOptions{
		FilePath: gitlab.Ptr(model.FilePath.ValueString()),
	}

	// Determine action type
	actionStr := "update"
	if !model.Action.IsNull() {
		actionStr = model.Action.ValueString()
	}

	switch actionStr {
	case "create":
		action.Action = gitlab.Ptr(gitlab.FileCreate)
	case "delete":
		action.Action = gitlab.Ptr(gitlab.FileDelete)
	case "move":
		action.Action = gitlab.Ptr(gitlab.FileMove)
	case "update":
		action.Action = gitlab.Ptr(gitlab.FileUpdate)
	case "chmod":
		action.Action = gitlab.Ptr(gitlab.FileChmod)
	default:
		return nil, fmt.Errorf("invalid action: %s", actionStr)
	}

	// Handle content
	if !model.Content.IsNull() && !model.ContentBase64.IsNull() {
		return nil, fmt.Errorf("cannot specify both content and content_base64")
	}

	if !model.Content.IsNull() {
		action.Content = gitlab.Ptr(model.Content.ValueString())
	} else if !model.ContentBase64.IsNull() {
		decoded, err := base64.StdEncoding.DecodeString(model.ContentBase64.ValueString())
		if err != nil {
			return nil, fmt.Errorf("failed to decode base64 content: %w", err)
		}
		action.Content = gitlab.Ptr(string(decoded))
		action.Encoding = gitlab.Ptr("base64")
	}

	// Override encoding if specified
	if !model.Encoding.IsNull() {
		action.Encoding = gitlab.Ptr(model.Encoding.ValueString())
	}

	return action, nil
}
