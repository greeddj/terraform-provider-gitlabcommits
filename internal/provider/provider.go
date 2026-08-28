// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/hashicorp/go-cleanhttp"
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

// responseHeaderTimeout bounds how long a request may wait for GitLab's
// response headers once the request, body included, has been sent, so a
// wedged instance or proxy fails the apply instead of hanging it forever.
// Uploads are not bounded by it, which matters for large commits; it only
// starts once GitLab has the whole request.
const responseHeaderTimeout = 5 * time.Minute

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
				Description: "Maximum number of retries on transient failures (5xx, 429) for read and probe requests. " +
					"The commit request (POST /repository/commits) is retried only on 429 and on connection failures that " +
					"happen before the request is sent, never on 5xx, so one apply cannot land two commits. " +
					"Default 5. Set to 0 to disable retries entirely.",
				Optional: true,
			},
			"retry_wait_min_ms": schema.Int64Attribute{
				Description: "Base wait (ms) between rate-limited (429) retries; it doubles with each attempt and the " +
					"Ratelimit-Reset header extends it when GitLab sends one. 5xx retries use the client's fixed 700-900 ms " +
					"schedule instead. Default 1000.",
				Optional: true,
			},
			"retry_wait_max_ms": schema.Int64Attribute{
				Description: "Bounds (ms) the random jitter added to each rate-limited (429) retry wait; the wait itself " +
					"is the growing base plus that jitter and can exceed this value. Default 30000.",
				Optional: true,
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
	// Unknown retry settings must not silently become defaults: the user set
	// them to something, and guessing here would hide a config wiring mistake.
	for _, a := range []struct {
		name  string
		value types.Int64
	}{
		{"max_retries", config.MaxRetries},
		{"retry_wait_min_ms", config.RetryWaitMinMs},
		{"retry_wait_max_ms", config.RetryWaitMaxMs},
	} {
		if a.value.IsUnknown() {
			resp.Diagnostics.AddAttributeError(
				path.Root(a.name),
				"Unknown retry configuration",
				a.name+" must be a known value at provider configure time.",
			)
		}
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
	// The token goes straight into a header; net/http rejects control
	// characters there with an opaque transport error, and a trailing newline
	// from a file or $(cat ...) is the usual way one gets in.
	if token != strings.TrimSpace(token) || strings.ContainsFunc(token, unicode.IsControl) {
		resp.Diagnostics.AddAttributeError(
			path.Root("token"),
			"Malformed GitLab API Token",
			"The token contains leading or trailing whitespace or a control character; check for a trailing newline if it came from a file or a shell substitution.",
		)
		return
	}

	clientOpts := []gitlab.ClientOptionFunc{}
	if baseURL != "" {
		// client-go only logs its own URL validation failure and carries on,
		// so a schemeless value would surface much later as a transport error.
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			resp.Diagnostics.AddAttributeError(
				path.Root("base_url"),
				"Invalid GitLab base URL",
				fmt.Sprintf("%q must be an absolute http:// or https:// URL with a host, for example https://gitlab.example.com", baseURL),
			)
			return
		}
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
	if !config.MaxRetries.IsNull() {
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
	if !config.RetryWaitMinMs.IsNull() {
		waitMin = config.RetryWaitMinMs.ValueInt64()
	}
	waitMax := int64(30000)
	if !config.RetryWaitMaxMs.IsNull() {
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

	// client-go's default is cleanhttp's pooled client with no response
	// timeout at all; keep the pooled transport and add one. The token
	// travels as a Private-Token header, which net/http does NOT strip when
	// following a cross-host redirect (unlike Authorization/Cookie), so
	// off-host redirects are refused by crossHostRedirectGuard.
	transport := cleanhttp.DefaultPooledTransport()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	clientOpts = append(clientOpts, gitlab.WithHTTPClient(&http.Client{
		Transport:     transport,
		CheckRedirect: crossHostRedirectGuard,
	}))

	client, err := gitlab.NewClient(token, clientOpts...)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create GitLab client", err.Error())
		return
	}

	tflog.Info(ctx, "GitLab Commits provider configured", map[string]any{
		"base_url":    baseURL,
		"max_retries": maxRetries,
	})

	resp.DataSourceData = client
	resp.ResourceData = &resourceDeps{
		client:       client,
		locks:        newBranchLocks(),
		retryCommits: maxRetries > 0,
	}
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
// redirects (including http->https upgrades) are allowed, but an https->http
// downgrade is refused - it would resend the token in cleartext. A refusal
// returns http.ErrUseLastResponse rather than an error: the 3xx then reaches
// client-go as a plain non-2xx response, which it does not retry (an error
// from CheckRedirect would be replayed max_retries times as a transport
// failure), and apiErrorDiag explains it with the Location header. The chain
// is capped at 10 to match net/http's default behaviour, which a non-nil
// CheckRedirect drops, and the cap is reported the same way.
func crossHostRedirectGuard(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 10 {
		// Same treatment as a refused hop: an error here would be replayed
		// by the client's retry for GET/HEAD, a 3xx is reported once.
		return http.ErrUseLastResponse
	}
	if req.URL.Host != via[0].URL.Host {
		return http.ErrUseLastResponse
	}
	if req.URL.Scheme == "http" && via[len(via)-1].URL.Scheme == "https" {
		return http.ErrUseLastResponse
	}
	return nil
}
