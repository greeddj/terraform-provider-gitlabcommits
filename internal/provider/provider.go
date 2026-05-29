// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

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

type gitlabCommitsProvider struct {
	version string
}

type gitlabCommitsProviderModel struct {
	Token          types.String `tfsdk:"token"`
	BaseURL        types.String `tfsdk:"base_url"`
	MaxRetries     types.Int64  `tfsdk:"max_retries"`
	RetryWaitMinMs types.Int64  `tfsdk:"retry_wait_min_ms"`
	RetryWaitMaxMs types.Int64  `tfsdk:"retry_wait_max_ms"`
}

func (p *gitlabCommitsProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "gitlabcommits"
	resp.Version = p.version
}

func (p *gitlabCommitsProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Terraform provider for managing repository files in GitLab via the Commits API. " +
			"Each managed resource produces one commit per terraform apply containing all of its file changes. " +
			"Tested against GitLab 18.x; older versions may work for basic operations but are not supported.",
		Attributes: map[string]schema.Attribute{
			"token": schema.StringAttribute{
				Description: "GitLab token used for REST API calls. Must have the `api` scope. " +
					"Personal, Project, or Group access tokens are supported; CI_JOB_TOKEN is not (its allowlist excludes POST /repository/commits). " +
					"May also be provided via the GITLAB_TOKEN environment variable. See the provider documentation's Authentication section for details.",
				Optional:  true,
				Sensitive: true,
			},
			"base_url": schema.StringAttribute{
				Description: "GitLab base URL for self-hosted instances. Defaults to https://gitlab.com. May also be provided via GITLAB_BASE_URL environment variable.",
				Optional:    true,
			},
			"max_retries": schema.Int64Attribute{
				Description: "Maximum number of retries on transient failures (5xx, 429). Default 5. Set to 0 to disable retries entirely.",
				Optional:    true,
			},
			"retry_wait_min_ms": schema.Int64Attribute{
				Description: "Lower bound (ms) of the exponential backoff between retries. Default 1000.",
				Optional:    true,
			},
			"retry_wait_max_ms": schema.Int64Attribute{
				Description: "Upper bound (ms) of the exponential backoff between retries. Default 30000.",
				Optional:    true,
			},
		},
	}
}

func (p *gitlabCommitsProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	tflog.Debug(ctx, "Configuring GitLab Commits provider")

	var config gitlabCommitsProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if config.Token.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("token"),
			"Unknown GitLab API Token",
			"Token must be a known value at provider configure time. Use a static value or `target apply` the source first.",
		)
	}
	if config.BaseURL.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("base_url"),
			"Unknown GitLab Base URL",
			"base_url must be a known value at provider configure time.",
		)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	token := os.Getenv("GITLAB_TOKEN")
	if !config.Token.IsNull() {
		token = config.Token.ValueString()
	}
	baseURL := os.Getenv("GITLAB_BASE_URL")
	if !config.BaseURL.IsNull() {
		baseURL = config.BaseURL.ValueString()
	}

	if token == "" {
		resp.Diagnostics.AddError(
			"Missing GitLab API Token",
			"Set `token` in the provider block or export GITLAB_TOKEN. The token must have the `api` scope (Personal, Project, or Group access token). See the provider documentation's Authentication section for details.",
		)
		return
	}

	clientOpts := []gitlab.ClientOptionFunc{}
	if baseURL != "" {
		clientOpts = append(clientOpts, gitlab.WithBaseURL(baseURL))
		if strings.HasPrefix(strings.ToLower(baseURL), "http://") {
			resp.Diagnostics.AddAttributeWarning(
				path.Root("base_url"),
				"GitLab base_url is using plaintext HTTP",
				"Traffic between Terraform and GitLab — including the API token — will be sent unencrypted. "+
					"Use https:// unless this is intentional (e.g. a TLS-terminating proxy in front of the API).",
			)
		}
	}

	maxRetries := int64(5)
	if !config.MaxRetries.IsNull() && !config.MaxRetries.IsUnknown() {
		maxRetries = config.MaxRetries.ValueInt64()
	}
	if maxRetries < 0 {
		resp.Diagnostics.AddAttributeError(path.Root("max_retries"), "Invalid value", "max_retries must be >= 0")
		return
	}
	if maxRetries == 0 {
		clientOpts = append(clientOpts, gitlab.WithoutRetries())
	} else {
		clientOpts = append(clientOpts, gitlab.WithCustomRetryMax(int(maxRetries)))
	}

	waitMin := int64(1000)
	if !config.RetryWaitMinMs.IsNull() && !config.RetryWaitMinMs.IsUnknown() {
		waitMin = config.RetryWaitMinMs.ValueInt64()
	}
	waitMax := int64(30000)
	if !config.RetryWaitMaxMs.IsNull() && !config.RetryWaitMaxMs.IsUnknown() {
		waitMax = config.RetryWaitMaxMs.ValueInt64()
	}
	if waitMin <= 0 || waitMax <= 0 || waitMin > waitMax {
		resp.Diagnostics.AddError("Invalid retry wait bounds",
			fmt.Sprintf("retry_wait_min_ms (%d) must be > 0 and <= retry_wait_max_ms (%d)", waitMin, waitMax))
		return
	}
	clientOpts = append(clientOpts, gitlab.WithCustomRetryWaitMinMax(
		time.Duration(waitMin)*time.Millisecond,
		time.Duration(waitMax)*time.Millisecond,
	))

	clientOpts = append(clientOpts, gitlab.WithUserAgent(
		fmt.Sprintf("terraform-provider-gitlabcommits/%s", p.version),
	))

	client, err := gitlab.NewClient(token, clientOpts...)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create GitLab client", err.Error())
		return
	}

	// The token travels as a Private-Token header, which net/http does NOT strip
	// when following a cross-host redirect (unlike Authorization/Cookie). Refuse
	// off-host redirects so a malicious or misconfigured GitLab cannot bounce the
	// token to another host.
	client.HTTPClient().CheckRedirect = crossHostRedirectGuard

	tflog.Info(ctx, "GitLab Commits provider configured", map[string]any{
		"base_url":    baseURL,
		"max_retries": maxRetries,
	})

	resp.DataSourceData = client
	resp.ResourceData = client
}

func (p *gitlabCommitsProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewFileDataSource,
		NewBranchHeadDataSource,
	}
}

func (p *gitlabCommitsProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewFilesResource,
	}
}

// crossHostRedirectGuard is the http.Client CheckRedirect policy for the GitLab
// client. net/http strips Authorization/Cookie on a cross-host redirect but
// leaves custom headers like Private-Token intact, so a 3xx pointing off-host
// would otherwise forward the API token to an attacker-controlled host. Same-host
// redirects (including http->https upgrades) are allowed; the chain is capped at
// 10 to match net/http's default behaviour, which a non-nil CheckRedirect drops.
func crossHostRedirectGuard(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("refusing cross-host redirect to %q: the GitLab token must not be sent to a host other than %q",
			req.URL.Host, via[0].URL.Host)
	}
	return nil
}
