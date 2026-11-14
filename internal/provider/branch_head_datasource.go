// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

var (
	_ datasource.DataSource              = &branchHeadDataSource{}
	_ datasource.DataSourceWithConfigure = &branchHeadDataSource{}
)

// NewBranchHeadDataSource returns a fresh instance of the branch_head data source.
func NewBranchHeadDataSource() datasource.DataSource {
	return &branchHeadDataSource{}
}

type branchHeadDataSource struct {
	client *gitlab.Client
}

type branchHeadModel struct {
	ProjectID types.String `tfsdk:"project_id"`
	Branch    types.String `tfsdk:"branch"`
	CommitSHA types.String `tfsdk:"commit_sha"`
	Protected types.Bool   `tfsdk:"protected"`
}

func (d *branchHeadDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_branch_head"
}

func (d *branchHeadDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads the head commit SHA and the protection status of a branch. " +
			"Terraform normally reads this data source during plan, before the commits of the same apply. " +
			"For the commit that a gitlabcommits_files resource makes, use the commit_sha of that resource: with depends_on, " +
			"the head can be a later commit.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{Required: true,
				Description: "The numeric ID or the full path of the project, for example \"group/subgroup/project\". " +
					"Do not URL-encode the path, because the provider encodes it.",
				Validators: []validator.String{stringNotEmpty()}},
			"branch": schema.StringAttribute{Required: true,
				Description: "The name of the branch. It must be a valid git ref name: for example, no space, \"..\" or \":\", " +
					"and no slash-separated part that ends with \".lock\". A tag or a commit SHA fails with \"Branch not found\".",
				Validators: []validator.String{stringNotEmpty(), stringBranchName()}},
			"commit_sha": schema.StringAttribute{Computed: true,
				Description: "The SHA of the head commit of the branch when Terraform reads the data source."},
			"protected": schema.BoolAttribute{Computed: true,
				Description: "True when the branch is a protected branch in GitLab."},
		},
	}
}

func (d *branchHeadDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*gitlab.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *gitlab.Client, got: %T", req.ProviderData))
		return
	}
	d.client = client
}

func (d *branchHeadDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data branchHeadModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	project := data.ProjectID.ValueString()
	branch := data.Branch.ValueString()

	b, _, err := d.client.Branches.GetBranch(project, branch, gitlab.WithContext(ctx))
	if err != nil {
		if errors.Is(err, gitlab.ErrNotFound) {
			resp.Diagnostics.AddError("Branch not found",
				fmt.Sprintf("branch %q in project %q was not found: the branch or the project does not exist, "+
					"or the token cannot see the project (GitLab answers 404 for missing access as well)", branch, project))
			return
		}
		summary, detail := apiErrorDiag("reading branch", project, branch, err)
		resp.Diagnostics.AddError(summary, detail)
		return
	}

	// GetBranch can return a branch whose Commit pointer is nil (omitted or
	// null in the response body) or whose commit has no id (an empty object);
	// guard before dereferencing so a hostile/buggy GitLab can neither panic
	// the provider nor hand an empty ref to whatever consumes commit_sha.
	if b == nil || b.Commit == nil || b.Commit.ID == "" {
		resp.Diagnostics.AddError("GitLab returned a branch with no commit",
			fmt.Sprintf("branch %q in project %q has no head commit in the API response", branch, project))
		return
	}

	data.CommitSHA = types.StringValue(b.Commit.ID)
	data.Protected = types.BoolValue(b.Protected)

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
