package provider

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// Ensure the implementation satisfies the expected interfaces.
var (
	_ resource.Resource                = &commitResource{}
	_ resource.ResourceWithConfigure   = &commitResource{}
	_ resource.ResourceWithImportState = &commitResource{}
)

// NewCommitResource is a helper function to simplify the provider implementation.
func NewCommitResource() resource.Resource {
	return &commitResource{}
}

// commitResource is the resource implementation.
type commitResource struct {
	client *gitlab.Client
}

// commitResourceModel maps the resource schema data.
type commitResourceModel struct {
	ID            types.String `tfsdk:"id"`
	ProjectID     types.String `tfsdk:"project_id"`
	Branch        types.String `tfsdk:"branch"`
	CommitMessage types.String `tfsdk:"commit_message"`
	Files         []fileModel  `tfsdk:"files"`
	AuthorEmail   types.String `tfsdk:"author_email"`
	AuthorName    types.String `tfsdk:"author_name"`
	CommitSHA     types.String `tfsdk:"commit_sha"`
}

// fileModel maps file data for commits.
type fileModel struct {
	FilePath      types.String `tfsdk:"file_path"`
	Content       types.String `tfsdk:"content"`
	ContentBase64 types.String `tfsdk:"content_base64"`
	Action        types.String `tfsdk:"action"`
	Encoding      types.String `tfsdk:"encoding"`
}

// Metadata returns the resource type name.
func (r *commitResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_commit"
}

// Schema defines the schema for the resource.
func (r *commitResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a GitLab commit with multiple file changes. All file changes are grouped into a single commit and push operation.",
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
			"commit_message": schema.StringAttribute{
				Description: "Commit message.",
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
			"files": schema.ListNestedAttribute{
				Description: "List of files to include in the commit.",
				Required:    true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"file_path": schema.StringAttribute{
							Description: "Full path to the file in the repository.",
							Required:    true,
						},
						"content": schema.StringAttribute{
							Description: "File content. Mutually exclusive with content_base64.",
							Optional:    true,
						},
						"content_base64": schema.StringAttribute{
							Description: "Base64 encoded file content. Mutually exclusive with content. Useful for binary files.",
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
					},
				},
			},
		},
	}
}

// Configure adds the provider configured client to the resource.
func (r *commitResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *commitResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Retrieve values from plan
	var plan commitResourceModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Build commit actions
	actions, err := r.buildCommitActions(plan.Files)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error Building Commit Actions",
			"Could not build commit actions: "+err.Error(),
		)
		return
	}

	// Create commit options
	commitOpts := &gitlab.CreateCommitOptions{
		Branch:        gitlab.Ptr(plan.Branch.ValueString()),
		CommitMessage: gitlab.Ptr(plan.CommitMessage.ValueString()),
		Actions:       actions,
	}

	if !plan.AuthorEmail.IsNull() {
		commitOpts.AuthorEmail = gitlab.Ptr(plan.AuthorEmail.ValueString())
	}

	if !plan.AuthorName.IsNull() {
		commitOpts.AuthorName = gitlab.Ptr(plan.AuthorName.ValueString())
	}

	// Create commit
	commit, _, err := r.client.Commits.CreateCommit(plan.ProjectID.ValueString(), commitOpts)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error Creating Commit",
			"Could not create commit: "+err.Error(),
		)
		return
	}

	// Set state
	plan.ID = types.StringValue(fmt.Sprintf("%s/%s/%s", plan.ProjectID.ValueString(), plan.Branch.ValueString(), commit.ID))
	plan.CommitSHA = types.StringValue(commit.ID)

	// Set state to fully populated data
	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
}

// Read refreshes the Terraform state with the latest data.
func (r *commitResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Get current state
	var state commitResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Get commit from GitLab
	commit, _, err := r.client.Commits.GetCommit(
		state.ProjectID.ValueString(),
		state.CommitSHA.ValueString(),
		nil,
	)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error Reading Commit",
			"Could not read commit SHA "+state.CommitSHA.ValueString()+": "+err.Error(),
		)
		return
	}

	// Update state
	state.CommitSHA = types.StringValue(commit.ID)

	// Set refreshed state
	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
}

// Update updates the resource and sets the updated Terraform state on success.
func (r *commitResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Retrieve values from plan
	var plan commitResourceModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Build commit actions
	actions, err := r.buildCommitActions(plan.Files)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error Building Commit Actions",
			"Could not build commit actions: "+err.Error(),
		)
		return
	}

	// Create commit options using the actions directly
	commitOpts := &gitlab.CreateCommitOptions{
		Branch:        gitlab.Ptr(plan.Branch.ValueString()),
		CommitMessage: gitlab.Ptr(plan.CommitMessage.ValueString()),
		Actions:       actions,
	}

	if !plan.AuthorEmail.IsNull() {
		commitOpts.AuthorEmail = gitlab.Ptr(plan.AuthorEmail.ValueString())
	}

	if !plan.AuthorName.IsNull() {
		commitOpts.AuthorName = gitlab.Ptr(plan.AuthorName.ValueString())
	}

	// Create commit
	commit, _, err := r.client.Commits.CreateCommit(
		plan.ProjectID.ValueString(),
		commitOpts,
	)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error Updating Commit",
			"Could not create new commit: "+err.Error(),
		)
		return
	}

	// Update state
	plan.CommitSHA = types.StringValue(commit.ID)

	// Set state
	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
}

// Delete deletes the resource and removes the Terraform state on success.
func (r *commitResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Retrieve values from state
	var state commitResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Note: We cannot actually delete a commit from GitLab
	// The resource will be removed from Terraform state only
	// If you want to revert changes, you need to create a new commit
}

// ImportState imports the resource state.
func (r *commitResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by ID format: project_id/branch/commit_sha
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// buildCommitActions converts file models to GitLab commit actions.
// It validates each file's action type, handles content encoding (text or base64),
// and ensures mutual exclusivity of content and content_base64 fields.
// Returns an error if validation fails for any file.
func (r *commitResource) buildCommitActions(files []fileModel) ([]*gitlab.CommitActionOptions, error) {
	actions := make([]*gitlab.CommitActionOptions, 0, len(files))

	for _, file := range files {
		action := &gitlab.CommitActionOptions{
			FilePath: gitlab.Ptr(file.FilePath.ValueString()),
		}

		// Determine action
		actionStr := "update"
		if !file.Action.IsNull() {
			actionStr = file.Action.ValueString()
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

		// Handle content - ensure mutual exclusivity of content types.
		if !file.Content.IsNull() && !file.ContentBase64.IsNull() {
			return nil, fmt.Errorf("cannot specify both content and content_base64 for file: %s", file.FilePath.ValueString())
		}

		if !file.Content.IsNull() {
			action.Content = gitlab.Ptr(file.Content.ValueString())
		} else if !file.ContentBase64.IsNull() {
			// Decode base64 content
			decoded, err := base64.StdEncoding.DecodeString(file.ContentBase64.ValueString())
			if err != nil {
				return nil, fmt.Errorf("failed to decode base64 content for file %s: %w", file.FilePath.ValueString(), err)
			}
			action.Content = gitlab.Ptr(string(decoded))
			action.Encoding = gitlab.Ptr("base64")
		}

		// Override encoding if specified
		if !file.Encoding.IsNull() {
			action.Encoding = gitlab.Ptr(file.Encoding.ValueString())
		}

		actions = append(actions, action)
	}

	return actions, nil
}
