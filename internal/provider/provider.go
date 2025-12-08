package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// Ensure the implementation satisfies the expected interfaces.
var (
	_ provider.Provider = &gitlabCommitsProvider{}
)

// New is a helper function to simplify provider server and testing implementation.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &gitlabCommitsProvider{
			version: version,
		}
	}
}

// gitlabCommitsProvider is the provider implementation.
type gitlabCommitsProvider struct {
	version string
}

// gitlabCommitsProviderModel maps provider schema data to a Go type.
type gitlabCommitsProviderModel struct {
	Token   types.String `tfsdk:"token"`
	BaseURL types.String `tfsdk:"base_url"`
}

// Metadata returns the provider type name.
func (p *gitlabCommitsProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "gitlabcommits"
	resp.Version = p.version
}

// Schema defines the provider-level schema for configuration data.
func (p *gitlabCommitsProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Terraform provider for GitLab Commits API. Allows creating commits with multiple files in a single push operation.",
		Attributes: map[string]schema.Attribute{
			"token": schema.StringAttribute{
				Description: "GitLab personal access token or project access token. May also be provided via GITLAB_TOKEN environment variable.",
				Optional:    true,
				Sensitive:   true,
			},
			"base_url": schema.StringAttribute{
				Description: "GitLab base URL for self-hosted instances. Defaults to https://gitlab.com. May also be provided via GITLAB_BASE_URL environment variable.",
				Optional:    true,
			},
		},
	}
}

// Configure prepares a GitLab API client for data sources and resources.
func (p *gitlabCommitsProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	// Retrieve provider data from configuration
	var config gitlabCommitsProviderModel
	diags := req.Config.Get(ctx, &config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// If practitioner provided a configuration value for any of the
	// attributes, it must be a known value.
	if config.Token.IsUnknown() {
		resp.Diagnostics.AddError(
			"Unknown GitLab API Token",
			"The provider cannot create the GitLab API client as there is an unknown configuration value for the GitLab API token. "+
				"Either target apply the source of the value first, set the value statically in the configuration, or use the GITLAB_TOKEN environment variable.",
		)
	}

	if config.BaseURL.IsUnknown() {
		resp.Diagnostics.AddError(
			"Unknown GitLab Base URL",
			"The provider cannot create the GitLab API client as there is an unknown configuration value for the GitLab Base URL. "+
				"Either target apply the source of the value first, set the value statically in the configuration, or use the GITLAB_BASE_URL environment variable.",
		)
	}

	if resp.Diagnostics.HasError() {
		return
	}

	// Default values to environment variables, but override
	// with Terraform configuration value if set.
	token := os.Getenv("GITLAB_TOKEN")
	baseURL := os.Getenv("GITLAB_BASE_URL")

	if !config.Token.IsNull() {
		token = config.Token.ValueString()
	}

	if !config.BaseURL.IsNull() {
		baseURL = config.BaseURL.ValueString()
	}

	// If any of the expected configurations are missing, return
	// errors with provider-specific guidance.
	if token == "" {
		resp.Diagnostics.AddError(
			"Missing GitLab API Token",
			"The provider cannot create the GitLab API client as there is a missing or empty value for the GitLab API token. "+
				"Set the token value in the configuration or use the GITLAB_TOKEN environment variable. "+
				"If either is already set, ensure the value is not empty.",
		)
	}

	if resp.Diagnostics.HasError() {
		return
	}

	// Create a new GitLab client using the configuration values
	var client *gitlab.Client
	var err error

	clientOpts := []gitlab.ClientOptionFunc{}

	if baseURL != "" {
		clientOpts = append(clientOpts, gitlab.WithBaseURL(baseURL))
	}

	client, err = gitlab.NewClient(token, clientOpts...)
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to Create GitLab API Client",
			"An unexpected error occurred when creating the GitLab API client. "+
				"If the error is not clear, please contact the provider developers.\n\n"+
				"GitLab Client Error: "+err.Error(),
		)
		return
	}

	// Make the GitLab client available during DataSource and Resource
	// type Configure methods.
	resp.DataSourceData = client
	resp.ResourceData = client
}

// DataSources defines the data sources implemented in the provider.
func (p *gitlabCommitsProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}

// Resources defines the resources implemented in the provider.
func (p *gitlabCommitsProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewCommitResource,
		NewFileResource,
	}
}
