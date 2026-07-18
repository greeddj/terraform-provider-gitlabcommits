// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

var (
	_ datasource.DataSource              = &fileDataSource{}
	_ datasource.DataSourceWithConfigure = &fileDataSource{}
)

// NewFileDataSource returns a fresh instance of the file data source.
func NewFileDataSource() datasource.DataSource {
	return &fileDataSource{}
}

type fileDataSource struct {
	client *gitlab.Client
}

type fileDataSourceModel struct {
	ProjectID       types.String `tfsdk:"project_id"`
	Branch          types.String `tfsdk:"branch"`
	FilePath        types.String `tfsdk:"file_path"`
	Content         types.String `tfsdk:"content"`
	ContentBase64   types.String `tfsdk:"content_base64"`
	BlobID          types.String `tfsdk:"blob_id"`
	LastCommitID    types.String `tfsdk:"last_commit_id"`
	Size            types.Int64  `tfsdk:"size"`
	ExecuteFilemode types.Bool   `tfsdk:"execute_filemode"`
}

func (d *fileDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file"
}

func (d *fileDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads a single file from a GitLab repository at the given branch ref. Useful for migration, " +
			"comparison against rendered HCL, or wiring up other resources with the file's commit metadata.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{
				Description: "Project ID or URL-encoded path.",
				Required:    true,
				Validators:  []validator.String{stringNotEmpty()},
			},
			"branch": schema.StringAttribute{
				Description: "Branch (or any ref) to read the file from.",
				Required:    true,
				Validators:  []validator.String{stringNotEmpty()},
			},
			"file_path": schema.StringAttribute{
				Description: "Path of the file inside the repository.",
				Required:    true,
				Validators:  []validator.String{stringNotEmpty()},
			},
			"content": schema.StringAttribute{
				Description: "Decoded text content of the file. Null when the file is not valid UTF-8 " +
					"(Terraform strings cannot hold arbitrary bytes without corruption); use content_base64 for binaries.",
				Computed: true,
			},
			"content_base64": schema.StringAttribute{
				Description: "Base64-encoded raw bytes of the file. Always set.",
				Computed:    true,
			},
			"blob_id": schema.StringAttribute{
				Description: "Opaque blob identifier returned by GitLab (git SHA-1 today, possibly SHA-256 on SHA-256 repositories).",
				Computed:    true,
			},
			"execute_filemode": schema.BoolAttribute{
				Description: "Whether the file has the executable bit set in the repo.",
				Computed:    true,
			},
			"last_commit_id": schema.StringAttribute{
				Description: "SHA of the most recent commit that touched this file.",
				Computed:    true,
			},
			"size": schema.Int64Attribute{
				Description: "Size of the file in bytes.",
				Computed:    true,
			},
		},
	}
}

func (d *fileDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *fileDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data fileDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	project := data.ProjectID.ValueString()
	branch := data.Branch.ValueString()
	path := data.FilePath.ValueString()

	file, _, err := d.client.RepositoryFiles.GetFile(project, path, &gitlab.GetFileOptions{
		Ref: gitlab.Ptr(branch),
	}, gitlab.WithContext(ctx))
	if err != nil {
		if errors.Is(err, gitlab.ErrNotFound) {
			resp.Diagnostics.AddError("File not found",
				fmt.Sprintf("file %q on branch %q in project %q does not exist", path, branch, project))
			return
		}
		summary, detail := apiErrorDiag(fmt.Sprintf("reading file %q", path), project, branch, err)
		resp.Diagnostics.AddError(summary, detail)
		return
	}

	raw, err := decodeRemoteContent(file)
	if err != nil {
		resp.Diagnostics.AddError("Cannot decode remote content", err.Error())
		return
	}

	// cty silently mangles invalid UTF-8 (bytes become U+FFFD), so a binary
	// file must surface as a null content, not corrupted-but-plausible text.
	if utf8.Valid(raw) {
		data.Content = types.StringValue(string(raw))
	} else {
		data.Content = types.StringNull()
	}
	data.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString(raw))
	// Same hostile-server ceiling as the resource: an absurd blob_id must not
	// bloat state through the data-source path either.
	if len(file.BlobID) > maxBlobIDLen {
		resp.Diagnostics.AddWarning("Ignoring oversized blob_id",
			fmt.Sprintf("file %q: server returned blob_id of unexpected length %d (max %d); leaving blob_id unset", path, len(file.BlobID), maxBlobIDLen))
		data.BlobID = types.StringNull()
	} else {
		data.BlobID = types.StringValue(file.BlobID)
	}
	data.ExecuteFilemode = types.BoolValue(file.ExecuteFilemode)
	data.LastCommitID = types.StringValue(file.LastCommitID)
	data.Size = types.Int64Value(file.Size)

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
