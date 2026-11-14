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
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
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
		Description: "Reads one file of a GitLab project at a branch, a tag or a commit SHA. " +
			"For a file in Git LFS, the data source reads the LFS pointer, not the LFS object. " +
			"To read the file after the commit of a gitlabcommits_files resource in the same apply, add that resource to depends_on.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{
				Description: "The numeric ID or the full path of the project, for example \"group/subgroup/project\". " +
					"Do not URL-encode the path, because the provider encodes it.",
				Required:   true,
				Validators: []validator.String{stringNotEmpty()},
			},
			"branch": schema.StringAttribute{
				Description: "The branch, the tag or the commit SHA to read the file from. It must be a valid git ref name: for example, " +
					"no space, \"..\" or \":\", and no slash-separated part that ends with \".lock\".",
				Required:   true,
				Validators: []validator.String{stringNotEmpty(), stringBranchName()},
			},
			"file_path": schema.StringAttribute{
				Description: "The path of the file, relative to the root of the repository. It must not start or end with a slash, " +
					"or contain \"//\", a \".\" or \"..\" part, or a part that starts with white space.",
				Required:   true,
				Validators: []validator.String{stringNotEmpty(), stringValidRepoPath()},
			},
			"content": schema.StringAttribute{
				Description: "The text of the file, or null when the bytes from GitLab are not valid UTF-8, for example in a binary file. " +
					"GitLab changes text in another encoding, for example UTF-16, to UTF-8. " +
					"Terraform changes text to Unicode NFC, so the value can differ from the bytes in content_base64.",
				Computed: true,
			},
			"content_base64": schema.StringAttribute{
				Description: "The bytes of the file from GitLab, encoded in base64 (never null). " +
					"These are the stored bytes for UTF-8 text and binary files, and the UTF-8 bytes for text in another encoding. " +
					"See the provider page, Limits.",
				Computed: true,
			},
			"blob_id": schema.StringAttribute{
				Description: "The ID of the git object of the file at the ref.",
				Computed:    true,
			},
			"execute_filemode": schema.BoolAttribute{
				Description: "True when the file has the executable bit at the ref.",
				Computed:    true,
			},
			"last_commit_id": schema.StringAttribute{
				Description: "The SHA of the last commit that changed the file, at the ref.",
				Computed:    true,
			},
			"size": schema.Int64Attribute{
				Description: "The size of the stored file in bytes. For text that GitLab changes to UTF-8, it can differ from " +
					"the number of decoded bytes in content_base64.",
				Computed: true,
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
		Ref: new(branch),
	}, gitlab.WithContext(ctx))
	if err != nil {
		if errors.Is(err, gitlab.ErrNotFound) {
			resp.Diagnostics.AddError("File not found",
				fmt.Sprintf("file %q at ref %q in project %q was not found: the file, the ref or the project does not exist, "+
					"or the token cannot see the project (GitLab answers 404 for missing access as well)", path, branch, project))
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

	// cty's JSON encoder writes invalid UTF-8 to the state file as U+FFFD,
	// so a binary file must surface as a null content, not
	// corrupted-but-plausible text.
	// Valid text is kept even where cty normalises it to NFC: canonically
	// equivalent text still serves jsondecode and the like, and
	// content_base64 carries the exact bytes.
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
