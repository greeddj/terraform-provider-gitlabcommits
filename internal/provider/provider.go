// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
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
	"github.com/hashicorp/go-retryablehttp"
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

// responseHeaderTimeout bounds only the wait for response headers after the
// whole request is sent. An upload, and a body that stalls after its headers,
// have no time limit.
const responseHeaderTimeout = 5 * time.Minute

// Upper bounds for the retry settings: a larger value is a typo. They also
// keep a millisecond count from wrapping time.Duration negative and
// int(maxRetries) from truncating on 32-bit builds.
const (
	maxMaxRetries  = 100
	maxRetryWaitMs = 3_600_000
)

// maxServerWait caps a 429 wait that GitLab asks for. A longer limit fails
// sooner through the doubling wait than by stalling the apply once per retry.
const maxServerWait = 5 * time.Minute

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
		Description: "The gitlabcommits provider manages files on GitLab branches through the GitLab Commits API. " +
			"Each create, update or destroy of a gitlabcommits_files resource makes a maximum of one commit, " +
			"with all its file changes. " +
			"A replacement can make two commits in one apply: a delete commit and a create commit.",
		Attributes: map[string]schema.Attribute{
			"token": schema.StringAttribute{
				Description: "The GitLab access token. " +
					"A fine-grained token needs Commit: Create, Repository: Read, Branch: Read and Project: Read, " +
					"and also Branch: Create if you set create_branch_from. " +
					"If token is null or empty, the provider uses GITLAB_TOKEN. " +
					"See the provider page, Authentication.",
				Optional:  true,
				Sensitive: true,
			},
			"base_url": schema.StringAttribute{
				Description: "The URL of the GitLab instance, with or without /api/v4, for example https://gitlab.example.com. " +
					"If base_url is null or empty, the provider uses GITLAB_BASE_URL, " +
					"or https://gitlab.com if neither has a value. " +
					"See the provider page, Authentication.",
				Optional: true,
			},
			"max_retries": schema.Int64Attribute{
				Description: "The maximum number of retries of a failed GitLab request, from 0 to 100. " +
					"The default is 5, and 0 stops all retries. " +
					"See the provider page, Limits.",
				Optional: true,
			},
			"retry_wait_min_ms": schema.Int64Attribute{
				Description: "The minimum wait in milliseconds before a retry after HTTP 429, from 1 to 3600000. " +
					"The default is 1000, and the value must not be more than retry_wait_max_ms. " +
					"See the provider page, Limits.",
				Optional: true,
			},
			"retry_wait_max_ms": schema.Int64Attribute{
				Description: "After HTTP 429, the provider adds a random time to each wait, " +
					"from 0 to retry_wait_max_ms minus retry_wait_min_ms milliseconds. " +
					"This value does not limit the wait. " +
					"It must be from 1 to 3600000 and not less than retry_wait_min_ms, with a default of 30000.",
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
			"base_url must be a known value at provider configure time. Use a static value, set GITLAB_BASE_URL and leave "+
				"base_url unset, or create the source first with `terraform apply -target=<address>`.",
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
				a.name+" must be a known value at provider configure time. Use a static value, or create the source "+
					"first with `terraform apply -target=<address>`. This setting has no environment variable.",
			)
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	// An empty string counts as unset: an empty base_url from a module
	// variable defaulting to "" would otherwise send a self-managed instance's
	// token to gitlab.com, client-go's default.
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
	// net/http rejects control characters in the token's header with an opaque
	// transport error. A trailing newline from a file or $(cat ...) is the
	// usual cause.
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
		// client-go only logs its own URL validation failure; a schemeless
		// value would surface later as a transport error. The value is not
		// echoed: it may carry credentials.
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
	clientOpts = append(clientOpts,
		gitlab.WithCustomRetryWaitMinMax(
			time.Duration(waitMin)*time.Millisecond,
			time.Duration(waitMax)*time.Millisecond,
		),
		gitlab.WithCustomBackoff(retryBackoff),
	)

	clientOpts = append(clientOpts, gitlab.WithUserAgent(
		fmt.Sprintf("terraform-provider-gitlabcommits/%s", p.version),
	))

	// client-go's default client has no response timeout. net/http keeps the
	// Private-Token header on a cross-host redirect, unlike Authorization, so
	// crossHostRedirectGuard refuses those.
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

// crossHostRedirectGuard refuses an off-host hop (net/http keeps
// Private-Token on it), an https->http downgrade and a changed method. It
// returns the 3xx rather than an error, which the retry would replay.
func crossHostRedirectGuard(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 10 {
		// net/http stops after 10 hops only without a custom CheckRedirect;
		// this restores that cap.
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

// isJobToken reports whether token is a CI job token: it matches
// jobTokenPattern or equals CI_JOB_TOKEN, which catches older unprefixed
// tokens and instance prefixes the pattern misses.
func isJobToken(token string) bool {
	if jobToken := os.Getenv("CI_JOB_TOKEN"); jobToken != "" && token == jobToken {
		return true
	}
	return jobTokenPattern.MatchString(token)
}

// withoutUserinfo renders u with its userinfo masked for the log: proxy
// credentials or a token in the user part must not reach a TF_LOG file that
// CI keeps.
func withoutUserinfo(u *url.URL) string {
	if u.User == nil {
		return u.String()
	}
	masked := *u
	masked.User = url.User("xxxxx")
	return masked.String()
}

// headerRateLimiter is the client's rate limiter. client-go would derive one
// from RateLimit-Limit into a plain field that concurrent requests race on;
// WithCustomLimiter stops that, and observe stores it atomically.
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

// observe configures the limiter once, from the first response it is handed,
// with client-go's formula. A first response without a usable
// RateLimit-Limit leaves the limiter off for good.
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

// rateLimitObserver is the transport under the GitLab client: it hands the
// headers of each response that may settle the limiter to it.
type rateLimitObserver struct {
	next    http.RoundTripper
	limiter *headerRateLimiter
}

func (t *rateLimitObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err == nil && settlesLimiter(resp) {
		t.limiter.observe(resp.Header)
	}
	return resp, err
}

// settlesLimiter reports whether resp may configure the rate limiter. A 3xx,
// 5xx or 429 without RateLimit-Limit is often a hop before the answer
// client-go reads, so a later response decides.
func settlesLimiter(resp *http.Response) bool {
	if resp.Header.Get("RateLimit-Limit") != "" {
		return true
	}
	code := resp.StatusCode
	return code < 300 || (code >= 400 && code < 500 && code != http.StatusTooManyRequests)
}

// CloseIdleConnections forwards to the pooled transport, whose idle pool
// http.Client reaches only through this method. go-retryablehttp calls it
// after a request finally fails, to drop suspect idle connections.
func (t *rateLimitObserver) CloseIdleConnections() {
	if c, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// retryBackoff is the client's backoff. client-go's WithRetryAfterBackoff
// waits for any Retry-After, up to a day for some limits; this one accepts a
// requested wait of at most maxServerWait.
func retryBackoff(minWait, maxWait time.Duration, attemptNum int, resp *http.Response) time.Duration {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		// client-go's schedule for a 5xx or a transport failure.
		return retryablehttp.LinearJitterBackoff(700*time.Millisecond, 900*time.Millisecond, attemptNum, resp)
	}
	wait, ok := requestedWait(resp.Header)
	if ok {
		wait = max(wait, minWait)
	} else {
		wait = doubled(minWait, attemptNum)
	}
	jitter := time.Duration(rand.Float64() * float64(max(maxWait-minWait, 0))) //nolint:gosec // jitter, not a secret
	if wait > math.MaxInt64-jitter {
		return math.MaxInt64
	}
	return wait + jitter
}

// requestedWait returns the wait a 429 asks for, if at most maxServerWait.
// Retry-After comes first: RateLimit-Reset is a Unix time, so it depends on
// the local clock.
func requestedWait(h http.Header) (time.Duration, bool) {
	wait, _ := retryAfter(h.Get("Retry-After"))
	if wait <= 0 {
		reset, err := strconv.ParseInt(h.Get("RateLimit-Reset"), 10, 64)
		if err != nil || reset <= 0 {
			return 0, false
		}
		wait = time.Until(time.Unix(reset, 0))
	}
	return wait, wait > 0 && wait <= maxServerWait
}

// retryAfter parses a Retry-After value, a number of seconds or an HTTP date,
// and reports whether it was a number of seconds. A negative number counts as
// zero.
func retryAfter(v string) (wait time.Duration, inSeconds bool) {
	if seconds, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Duration(min(max(seconds, 0), int64(math.MaxInt64/time.Second))) * time.Second, true
	}
	if date, err := http.ParseTime(v); err == nil {
		return time.Until(date), false
	}
	return 0, false
}

// doubled returns d doubled n times, saturating instead of wrapping negative.
func doubled(d time.Duration, n int) time.Duration {
	for range n {
		if d > math.MaxInt64/2 {
			return math.MaxInt64
		}
		d *= 2
	}
	return d
}
