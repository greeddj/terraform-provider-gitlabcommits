// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
	"golang.org/x/time/rate"
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

// Upper bounds for the retry settings. A larger value is a typo rather than a
// policy: a wait of days that only a cancelled run ends, a millisecond count
// that wraps time.Duration negative (429 retries then fire back to back), or
// a max_retries that the int conversion truncates on the 32-bit release
// builds, silently disabling retries.
const (
	maxMaxRetries  = 100
	maxRetryWaitMs = 3_600_000
)

// jobTokenPattern matches a CI job token: "glcbt-", after the instance token
// prefix and its hyphen when the instance sets one (GitLab allows up to 20
// alphanumerics there).
var jobTokenPattern = regexp.MustCompile(`^(?:[A-Za-z0-9]{1,20}-)?glcbt-`)

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
			"Tested against GitLab 19.x; older versions may work for basic operations but are not supported.",
		Attributes: map[string]schema.Attribute{
			"token": schema.StringAttribute{
				Description: "GitLab token used for REST API calls: a Personal, Project, or Group access token with the `api` scope, " +
					"or a fine-grained personal access token (GitLab 19.2+) with Commit: Create, Repository: Read, Branch: Read and " +
					"Project: Read (plus Branch: Create when create_branch_from is used). A CI job token (CI_JOB_TOKEN) is rejected: the provider " +
					"authenticates with the Private-Token header, which GitLab does not accept for a job token, and the job-token " +
					"allowlist leaves out POST /repository/commits anyway. " +
					"When unset or empty, the GITLAB_TOKEN environment variable is used. See the provider documentation's Authentication section for details.",
				Optional:  true,
				Sensitive: true,
			},
			"base_url": schema.StringAttribute{
				Description: "GitLab base URL for self-hosted instances. When unset or empty, the GITLAB_BASE_URL environment " +
					"variable is used, and https://gitlab.com when that is unset or empty too.",
				Optional: true,
			},
			"max_retries": schema.Int64Attribute{
				Description: "Maximum number of retries on transient failures (5xx, 429) for read and probe requests. " +
					"The commit request (POST /repository/commits) is retried only on 429 and on connection failures that " +
					"happen before the request is sent, never on 5xx, so one apply cannot land two commits. " +
					"Default 5, at most 100. Set to 0 to disable retries entirely.",
				Optional: true,
			},
			"retry_wait_min_ms": schema.Int64Attribute{
				Description: "Base wait (ms) for rate-limited (429) retries. When GitLab sends a RateLimit-Reset header the " +
					"wait lasts until the reset, or this value if that is longer; without the header it doubles with each " +
					"attempt. 5xx and connection retries use client-go's linear schedule instead: 700-900 ms times the " +
					"attempt number. Default 1000, at most 3600000 (one hour).",
				Optional: true,
			},
			"retry_wait_max_ms": schema.Int64Attribute{
				Description: "Sets the random jitter (ms) added to each rate-limited (429) retry wait, which is up to " +
					"retry_wait_max_ms - retry_wait_min_ms; the total wait can exceed this value. Must not be below " +
					"retry_wait_min_ms. Default 30000, at most 3600000 (one hour).",
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
			"Token must be a known value at provider configure time. Use a static value, set GITLAB_TOKEN and leave token "+
				"unset, or create the source first with `terraform apply -target=<address>`.",
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

	// An empty string counts as unset, so a module variable defaulting to ""
	// falls back to the environment instead of overriding it: an empty
	// base_url would otherwise send a self-managed instance's token to
	// gitlab.com, client-go's default.
	token := os.Getenv("GITLAB_TOKEN")
	if v := config.Token.ValueString(); v != "" {
		token = v
	}
	baseURL, baseURLSource := os.Getenv("GITLAB_BASE_URL"), "GITLAB_BASE_URL"
	if v := config.BaseURL.ValueString(); v != "" {
		baseURL, baseURLSource = v, "base_url"
	}

	if token == "" {
		resp.Diagnostics.AddError(
			"Missing GitLab API Token",
			"Set `token` in the provider block or export GITLAB_TOKEN. The token must have the `api` scope (Personal, Project, or Group access token) or be a fine-grained personal access token with the permissions listed in the provider documentation's Authentication section.",
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
	if isJobToken(token) {
		resp.Diagnostics.AddAttributeError(
			path.Root("token"),
			"CI job tokens are not supported",
			"The token is a CI job token (CI_JOB_TOKEN). The provider authenticates with the Private-Token header, which "+
				"GitLab does not accept for a job token: depending on the version it answers 401 or ignores the token and "+
				"runs the request anonymously. The job-token allowlist leaves out POST /repository/commits as well. Use a "+
				"Personal, Project or Group access token with the `api` scope, or a fine-grained personal access token; "+
				"see the provider documentation's Authentication section.",
		)
		return
	}

	clientOpts := []gitlab.ClientOptionFunc{}
	logURL := ""
	if baseURL != "" {
		// client-go only logs its own URL validation failure and carries on,
		// so a schemeless value would surface much later as a transport error.
		// The value is not echoed: it may carry a proxy's credentials.
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			resp.Diagnostics.AddAttributeError(
				path.Root("base_url"),
				"Invalid GitLab base URL",
				fmt.Sprintf("The base URL from %s must be an absolute http:// or https:// URL with a host, for example "+
					"https://gitlab.example.com", baseURLSource),
			)
			return
		}
		logURL = withoutUserinfo(u)
		clientOpts = append(clientOpts, gitlab.WithBaseURL(baseURL))
		if strings.HasPrefix(strings.ToLower(baseURL), "http://") {
			resp.Diagnostics.AddAttributeWarning(
				path.Root("base_url"),
				"GitLab base_url is using plaintext HTTP",
				"Traffic between Terraform and GitLab - including the API token - will be sent unencrypted. "+
					"Use https:// unless this is intentional (e.g. a TLS-terminating proxy in front of the API).",
			)
		}
	}

	maxRetries := int64(5)
	if !config.MaxRetries.IsNull() {
		maxRetries = config.MaxRetries.ValueInt64()
	}
	if maxRetries < 0 || maxRetries > maxMaxRetries {
		resp.Diagnostics.AddAttributeError(path.Root("max_retries"), "Invalid value",
			fmt.Sprintf("max_retries must be between 0 and %d", maxMaxRetries))
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
	for _, w := range []struct {
		name  string
		value int64
	}{
		{"retry_wait_min_ms", waitMin},
		{"retry_wait_max_ms", waitMax},
	} {
		if w.value > maxRetryWaitMs {
			resp.Diagnostics.AddAttributeError(path.Root(w.name), "Invalid value",
				fmt.Sprintf("%s must be at most %d (one hour)", w.name, maxRetryWaitMs))
		}
	}
	if resp.Diagnostics.HasError() {
		return
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
	limiter := newHeaderRateLimiter()
	clientOpts = append(clientOpts,
		gitlab.WithHTTPClient(&http.Client{
			Transport:     &rateLimitObserver{next: transport, limiter: limiter},
			CheckRedirect: crossHostRedirectGuard,
		}),
		gitlab.WithCustomLimiter(limiter),
	)

	client, err := gitlab.NewClient(token, clientOpts...)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create GitLab client", err.Error())
		return
	}

	tflog.Info(ctx, "GitLab Commits provider configured", map[string]any{
		"base_url":    logURL,
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
// downgrade is refused - it would resend the token in cleartext. So is a hop
// on which net/http changed the method: on 301/302/303 it turns a POST into
// a body-less GET, which would read the list at the commit or branch URL and
// fail as a JSON decode error; a 307/308 keeps the method and the body and is
// followed. A refusal returns http.ErrUseLastResponse rather than an error:
// the 3xx then reaches client-go as a plain non-2xx response, which it does
// not retry (an error from CheckRedirect would be replayed max_retries times
// as a transport failure), and apiErrorDiag explains it with the Location
// header. The chain is capped at 10 to match net/http's default behaviour,
// which a non-nil CheckRedirect drops, and the cap is reported the same way.
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
	if req.Method != via[len(via)-1].Method {
		return http.ErrUseLastResponse
	}
	return nil
}

// isJobToken reports whether token is a CI job token, by its prefix or, for
// a job token from before the prefix or an instance prefix the pattern
// misses, by being the job's own CI_JOB_TOKEN.
func isJobToken(token string) bool {
	if jobToken := os.Getenv("CI_JOB_TOKEN"); jobToken != "" && token == jobToken {
		return true
	}
	return jobTokenPattern.MatchString(token)
}

// withoutUserinfo renders u with its userinfo masked, for the log: a
// basic-auth proxy's credentials, or a token in the user part as git URLs
// carry one, must not reach a TF_LOG file that CI keeps.
func withoutUserinfo(u *url.URL) string {
	if u.User == nil {
		return u.String()
	}
	masked := *u
	masked.User = url.User("xxxxx")
	return masked.String()
}

// headerRateLimiter is the GitLab client's rate limiter. client-go derives
// one itself from the first response's RateLimit-Limit header, but stores it
// in a plain field that every request reads without synchronisation: a data
// race once requests run concurrently, as the refresh fan-outs and parallel
// resource instances sharing the client do. Installing this limiter with
// gitlab.WithCustomLimiter turns that derivation off; observe repeats it and
// publishes the result through an atomic pointer.
type headerRateLimiter struct {
	current atomic.Pointer[rate.Limiter]
	once    sync.Once
}

func newHeaderRateLimiter() *headerRateLimiter {
	l := &headerRateLimiter{}
	l.current.Store(rate.NewLimiter(rate.Inf, 0))
	return l
}

func (l *headerRateLimiter) Wait(ctx context.Context) error {
	return l.current.Load().Wait(ctx)
}

// observe configures the limiter from the first response, as client-go
// does: two thirds of GitLab's per-minute limit as the steady rate and a
// third as the burst, with the request that brought the header counted. A
// first response without a usable header leaves the limiter off for good.
func (l *headerRateLimiter) observe(h http.Header) {
	l.once.Do(func() {
		perMinute, _ := strconv.ParseFloat(h.Get("RateLimit-Limit"), 64)
		if perMinute > 0 {
			perSecond := perMinute / 60
			// The cap keeps a huge value from converting to a negative burst,
			// which would fail every request.
			lim := rate.NewLimiter(rate.Limit(perSecond*0.66), max(1, int(min(perSecond*0.33, math.MaxInt32))))
			lim.Allow()
			l.current.Store(lim)
		}
	})
}

// rateLimitObserver is the transport under the GitLab client: it hands each
// response's headers to the rate limiter.
type rateLimitObserver struct {
	next    http.RoundTripper
	limiter *headerRateLimiter
}

func (t *rateLimitObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err == nil {
		t.limiter.observe(resp.Header)
	}
	return resp, err
}

// CloseIdleConnections forwards to the pooled transport. http.Client reaches
// a transport's idle pool only through this method, and go-retryablehttp
// calls it whenever a request finally fails or is cancelled, to drop idle
// connections that may be as broken as the one that failed; without the
// forward the pool would keep them.
func (t *rateLimitObserver) CloseIdleConnections() {
	if c, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
