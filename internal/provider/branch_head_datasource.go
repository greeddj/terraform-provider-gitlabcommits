// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
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
		Description: "Returns the SHA at the head of a branch. Useful for wiring up downstream pipelines or " +
			"populating last_commit_id when bootstrapping optimistic locking.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{Required: true,
				Description: "Project ID or URL-encoded path."},
			"branch": schema.StringAttribute{Required: true,
				Description: "Branch name."},
			"commit_sha": schema.StringAttribute{Computed: true,
				Description: "SHA of the commit at the branch head."},
			"protected": schema.BoolAttribute{Computed: true,
				Description: "Whether the branch is protected in GitLab."},
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
				fmt.Sprintf("branch %q does not exist in project %q", branch, project))
			return
		}
		summary, detail := apiErrorDiag("reading branch", project, branch, err)
		resp.Diagnostics.AddError(summary, detail)
		return
	}

	data.CommitSHA = types.StringValue(b.Commit.ID)
	data.Protected = types.BoolValue(b.Protected)

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
