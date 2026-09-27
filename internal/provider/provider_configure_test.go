// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
	"golang.org/x/time/rate"
)

// runConfigure invokes the provider's Configure with the given attribute
// overrides (any attribute not supplied defaults to null).
func runConfigure(t *testing.T, attrs map[string]tftypes.Value) *provider.ConfigureResponse {
	t.Helper()
	return runConfigureCtx(t.Context(), t, attrs)
}

// runConfigureCtx is runConfigure with the context Configure runs in, for a
// test that captures what it logs.
func runConfigureCtx(ctx context.Context, t *testing.T, attrs map[string]tftypes.Value) *provider.ConfigureResponse {
	t.Helper()
	p := New("test")()

	sresp := &provider.SchemaResponse{}
	p.Schema(ctx, provider.SchemaRequest{}, sresp)
	sch := sresp.Schema

	vals := map[string]tftypes.Value{
		"token":             tftypes.NewValue(tftypes.String, nil),
		"base_url":          tftypes.NewValue(tftypes.String, nil),
		"max_retries":       tftypes.NewValue(tftypes.Number, nil),
		"retry_wait_min_ms": tftypes.NewValue(tftypes.Number, nil),
		"retry_wait_max_ms": tftypes.NewValue(tftypes.Number, nil),
	}
	maps.Copy(vals, attrs)
	raw := tftypes.NewValue(sch.Type().TerraformType(ctx), vals)

	req := provider.ConfigureRequest{Config: tfsdk.Config{Schema: sch, Raw: raw}}
	resp := &provider.ConfigureResponse{}
	p.Configure(ctx, req, resp)
	return resp
}

func TestConfigure_MissingTokenErrors(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_BASE_URL", "")
	resp := runConfigure(t, nil)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error when no token is provided")
	}
}

// TestConfigure_PlaintextHTTPWarnsAndWiresRedirectGuard covers the http:// warning
// and asserts the cross-host redirect guard is installed on the client.
func TestConfigure_PlaintextHTTPWarnsAndWiresRedirectGuard(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_BASE_URL", "")
	resp := runConfigure(t, map[string]tftypes.Value{
		"token":    tftypes.NewValue(tftypes.String, "tok"),
		"base_url": tftypes.NewValue(tftypes.String, "http://gl.example.com"),
	})
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if resp.Diagnostics.WarningsCount() == 0 {
		t.Error("expected a plaintext-HTTP warning")
	}
	deps, ok := resp.ResourceData.(*resourceDeps)
	if !ok || deps == nil || deps.client == nil {
		t.Fatalf("expected configured *resourceDeps with a client, got %T", resp.ResourceData)
	}
	if deps.client.HTTPClient().CheckRedirect == nil {
		t.Error("expected the cross-host redirect guard to be installed on the client")
	}
	if deps.locks == nil {
		t.Error("expected the branch locks to be wired for resources")
	}
	if !deps.retryCommits {
		t.Error("expected commit retries to be enabled with the default max_retries")
	}
	if client, ok := resp.DataSourceData.(*gitlab.Client); !ok || client == nil {
		t.Errorf("expected data sources to receive the *gitlab.Client, got %T", resp.DataSourceData)
	}
}

// TestConfigure_WiresEveryResourceAndDataSource hands what the provider's
// Configure returns to every resource and data source it registers, as the
// framework does. Each must accept its own kind of provider data, reject the
// other kind, and ignore nil (the framework passes nil before the provider
// is configured). The resource must end up with the shared client, this
// configuration's branch locks and its commit retry setting; the data sources
// with the shared client.
func TestConfigure_WiresEveryResourceAndDataSource(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_BASE_URL", "")
	retries := map[string]tftypes.Value{
		"default max_retries": tftypes.NewValue(tftypes.Number, nil),
		"max_retries = 0":     tftypes.NewValue(tftypes.Number, big.NewFloat(0)),
	}
	for name, maxRetries := range retries {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			cfg := runConfigure(t, map[string]tftypes.Value{
				"token":       tftypes.NewValue(tftypes.String, "tok"),
				"max_retries": maxRetries,
			})
			if cfg.Diagnostics.HasError() {
				t.Fatalf("configure: %v", cfg.Diagnostics.Errors())
			}
			deps := cfg.ResourceData.(*resourceDeps)
			p := New("test")()

			resources := p.Resources(ctx)
			if len(resources) != 1 {
				t.Fatalf("resources = %d, want 1", len(resources))
			}
			for _, newResource := range resources {
				r, ok := newResource().(*filesResource)
				if !ok {
					t.Fatalf("unexpected resource type %T", newResource())
				}
				resp := &resource.ConfigureResponse{}
				r.Configure(ctx, resource.ConfigureRequest{ProviderData: cfg.DataSourceData}, resp)
				if !resp.Diagnostics.HasError() || r.client != nil {
					t.Errorf("the resource must reject the data sources' *gitlab.Client, got %v", resp.Diagnostics)
				}
				resp = &resource.ConfigureResponse{}
				r.Configure(ctx, resource.ConfigureRequest{}, resp)
				if resp.Diagnostics.HasError() || r.client != nil {
					t.Errorf("the resource must ignore nil provider data, got %v", resp.Diagnostics)
				}
				r.Configure(ctx, resource.ConfigureRequest{ProviderData: cfg.ResourceData}, resp)
				if resp.Diagnostics.HasError() {
					t.Fatalf("configure resource: %v", resp.Diagnostics.Errors())
				}
				if r.client != deps.client || r.locks != deps.locks || r.retryCommits != deps.retryCommits {
					t.Errorf("resource wiring = client %p locks %p retry %v, want %p %p %v",
						r.client, r.locks, r.retryCommits, deps.client, deps.locks, deps.retryCommits)
				}
			}

			seen := map[string]bool{}
			for _, newDataSource := range p.DataSources(ctx) {
				d := newDataSource()
				configurable, ok := d.(datasource.DataSourceWithConfigure)
				if !ok {
					t.Fatalf("data source %T has no Configure", d)
				}
				client := func() *gitlab.Client {
					switch ds := d.(type) {
					case *fileDataSource:
						return ds.client
					case *branchHeadDataSource:
						return ds.client
					}
					t.Fatalf("unexpected data source type %T", d)
					return nil
				}
				resp := &datasource.ConfigureResponse{}
				configurable.Configure(ctx, datasource.ConfigureRequest{ProviderData: cfg.ResourceData}, resp)
				if !resp.Diagnostics.HasError() || client() != nil {
					t.Errorf("%T must reject *resourceDeps, got %v", d, resp.Diagnostics)
				}
				resp = &datasource.ConfigureResponse{}
				configurable.Configure(ctx, datasource.ConfigureRequest{}, resp)
				if resp.Diagnostics.HasError() || client() != nil {
					t.Errorf("%T must ignore nil provider data, got %v", d, resp.Diagnostics)
				}
				configurable.Configure(ctx, datasource.ConfigureRequest{ProviderData: cfg.DataSourceData}, resp)
				if resp.Diagnostics.HasError() {
					t.Fatalf("configure %T: %v", d, resp.Diagnostics.Errors())
				}
				if client() != deps.client {
					t.Errorf("%T client = %p, want the shared %p", d, client(), deps.client)
				}
				seen[fmt.Sprintf("%T", d)] = true
			}
			if len(seen) != 2 {
				t.Errorf("data sources = %v, want the file and branch_head data sources", seen)
			}
		})
	}
}

// tokenCapture is a fake GitLab that records the token of the last request
// and answers a branch lookup.
func tokenCapture(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("PRIVATE-TOKEN")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"head"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func configuredClient(t *testing.T, attrs map[string]tftypes.Value) *gitlab.Client {
	t.Helper()
	resp := runConfigure(t, attrs)
	if resp.Diagnostics.HasError() {
		t.Fatalf("configure: %v", resp.Diagnostics.Errors())
	}
	return resp.ResourceData.(*resourceDeps).client
}

func TestConfigure_TokenSourcing(t *testing.T) {
	t.Run("from environment", func(t *testing.T) {
		srv, seen := tokenCapture(t)
		t.Setenv("GITLAB_TOKEN", "envtok")
		t.Setenv("GITLAB_BASE_URL", "")
		client := configuredClient(t, map[string]tftypes.Value{
			"base_url": tftypes.NewValue(tftypes.String, srv.URL),
		})
		if _, _, err := client.Branches.GetBranch("proj", "main", gitlab.WithContext(t.Context())); err != nil {
			t.Fatalf("request: %v", err)
		}
		if *seen != "envtok" {
			t.Errorf("token sent = %q, want the environment token", *seen)
		}
	})
	t.Run("config beats environment", func(t *testing.T) {
		srv, seen := tokenCapture(t)
		t.Setenv("GITLAB_TOKEN", "envtok")
		client := configuredClient(t, map[string]tftypes.Value{
			"token":    tftypes.NewValue(tftypes.String, "cfgtok"),
			"base_url": tftypes.NewValue(tftypes.String, srv.URL),
		})
		if _, _, err := client.Branches.GetBranch("proj", "main", gitlab.WithContext(t.Context())); err != nil {
			t.Fatalf("request: %v", err)
		}
		if *seen != "cfgtok" {
			t.Errorf("token sent = %q, want the configured token", *seen)
		}
	})
	t.Run("base_url from environment", func(t *testing.T) {
		srv, _ := tokenCapture(t)
		t.Setenv("GITLAB_TOKEN", "envtok")
		t.Setenv("GITLAB_BASE_URL", srv.URL)
		client := configuredClient(t, map[string]tftypes.Value{})
		if got := client.BaseURL().String(); !strings.HasPrefix(got, srv.URL) {
			t.Errorf("base URL = %q, want it under %q", got, srv.URL)
		}
	})
}

func TestConfigure_RejectsMalformedInputs(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_BASE_URL", "")
	cases := map[string]map[string]tftypes.Value{
		"token with trailing newline": {"token": tftypes.NewValue(tftypes.String, "tok\n")},
		"token with leading space":    {"token": tftypes.NewValue(tftypes.String, " tok")},
		"base_url without scheme": {
			"token":    tftypes.NewValue(tftypes.String, "tok"),
			"base_url": tftypes.NewValue(tftypes.String, "gitlab.example.com"),
		},
		"base_url with odd scheme": {
			"token":    tftypes.NewValue(tftypes.String, "tok"),
			"base_url": tftypes.NewValue(tftypes.String, "ftp://gitlab.example.com"),
		},
	}
	for name, attrs := range cases {
		t.Run(name, func(t *testing.T) {
			resp := runConfigure(t, attrs)
			if !resp.Diagnostics.HasError() {
				t.Fatal("expected a configure error")
			}
		})
	}
	t.Run("base_url with /api/v4 suffix is accepted", func(t *testing.T) {
		client := configuredClient(t, map[string]tftypes.Value{
			"token":    tftypes.NewValue(tftypes.String, "tok"),
			"base_url": tftypes.NewValue(tftypes.String, "https://gitlab.example.com/api/v4"),
		})
		if got := client.BaseURL().String(); got != "https://gitlab.example.com/api/v4/" {
			t.Errorf("base URL = %q", got)
		}
	})
}

func TestConfigure_ResponseHeaderTimeout(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_BASE_URL", "")
	client := configuredClient(t, map[string]tftypes.Value{"token": tftypes.NewValue(tftypes.String, "tok")})
	observer, ok := client.HTTPClient().Transport.(*rateLimitObserver)
	if !ok {
		t.Fatalf("transport is %T, want *rateLimitObserver", client.HTTPClient().Transport)
	}
	transport, ok := observer.next.(*http.Transport)
	if !ok {
		t.Fatalf("transport under the rate-limit observer is %T, want *http.Transport", observer.next)
	}
	if transport.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", transport.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	if transport.MaxIdleConnsPerHost == 0 {
		t.Error("expected cleanhttp's pooled transport settings to be kept")
	}
}

// closeIdleRecorder is a transport that records CloseIdleConnections.
type closeIdleRecorder struct {
	http.RoundTripper
	closed bool
}

func (r *closeIdleRecorder) CloseIdleConnections() { r.closed = true }

// TestConfigure_CloseIdleConnectionsReachesThePool: the retrying client
// flushes the idle pool when a request finally fails, through
// http.Client.CloseIdleConnections, which does nothing unless the
// transport implements the method; the rate-limit observer must pass it on.
func TestConfigure_CloseIdleConnectionsReachesThePool(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_BASE_URL", "")
	client := configuredClient(t, map[string]tftypes.Value{"token": tftypes.NewValue(tftypes.String, "tok")})
	observer, ok := client.HTTPClient().Transport.(*rateLimitObserver)
	if !ok {
		t.Fatalf("transport is %T, want *rateLimitObserver", client.HTTPClient().Transport)
	}
	recorder := &closeIdleRecorder{RoundTripper: observer.next}
	observer.next = recorder

	client.HTTPClient().CloseIdleConnections()

	if !recorder.closed {
		t.Error("CloseIdleConnections did not reach the pooled transport")
	}
}

// TestConfigure_ZeroRetriesDisablesCommitRetryPolicy: max_retries = 0 must
// switch the commit-specific retry policy off too, or it would bypass
// WithoutRetries on the shared client.
func TestConfigure_ZeroRetriesDisablesCommitRetryPolicy(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	resp := runConfigure(t, map[string]tftypes.Value{
		"token":       tftypes.NewValue(tftypes.String, "tok"),
		"max_retries": tftypes.NewValue(tftypes.Number, big.NewFloat(0)),
	})
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	deps, ok := resp.ResourceData.(*resourceDeps)
	if !ok {
		t.Fatalf("expected *resourceDeps, got %T", resp.ResourceData)
	}
	if deps.retryCommits {
		t.Error("max_retries = 0 must disable the commit retry policy")
	}
}

// TestConfigure_InvalidRetryBoundsError: retry settings out of range are
// rejected, the caps included: beyond them a wait wraps time.Duration
// negative or lasts days, and max_retries truncates on a 32-bit int.
func TestConfigure_InvalidRetryBoundsError(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	cases := map[string]map[string]tftypes.Value{
		"min greater than max": {
			"token":             tftypes.NewValue(tftypes.String, "tok"),
			"retry_wait_min_ms": tftypes.NewValue(tftypes.Number, big.NewFloat(5000)),
			"retry_wait_max_ms": tftypes.NewValue(tftypes.Number, big.NewFloat(1000)),
		},
		"negative max_retries": {
			"token":       tftypes.NewValue(tftypes.String, "tok"),
			"max_retries": tftypes.NewValue(tftypes.Number, big.NewFloat(-1)),
		},
		"max_retries above the cap": {
			"token":       tftypes.NewValue(tftypes.String, "tok"),
			"max_retries": tftypes.NewValue(tftypes.Number, big.NewFloat(101)),
		},
		"max_retries that truncates on 32-bit int": {
			"token":       tftypes.NewValue(tftypes.String, "tok"),
			"max_retries": tftypes.NewValue(tftypes.Number, big.NewFloat(4294967296)),
		},
		"retry_wait_max_ms above the cap": {
			"token":             tftypes.NewValue(tftypes.String, "tok"),
			"retry_wait_max_ms": tftypes.NewValue(tftypes.Number, big.NewFloat(3600001)),
		},
		"retry_wait_max_ms that wraps time.Duration": {
			"token":             tftypes.NewValue(tftypes.String, "tok"),
			"retry_wait_max_ms": tftypes.NewValue(tftypes.Number, big.NewFloat(10000000000000)),
		},
		"retry_wait_min_ms above the cap": {
			"token":             tftypes.NewValue(tftypes.String, "tok"),
			"retry_wait_min_ms": tftypes.NewValue(tftypes.Number, big.NewFloat(3600001)),
			"retry_wait_max_ms": tftypes.NewValue(tftypes.Number, big.NewFloat(3600001)),
		},
	}
	for name, attrs := range cases {
		t.Run(name, func(t *testing.T) {
			if resp := runConfigure(t, attrs); !resp.Diagnostics.HasError() {
				t.Fatal("expected an error diagnostic")
			}
		})
	}
	t.Run("the caps themselves are accepted", func(t *testing.T) {
		configuredClient(t, map[string]tftypes.Value{
			"token":             tftypes.NewValue(tftypes.String, "tok"),
			"max_retries":       tftypes.NewValue(tftypes.Number, big.NewFloat(maxMaxRetries)),
			"retry_wait_min_ms": tftypes.NewValue(tftypes.Number, big.NewFloat(maxRetryWaitMs)),
			"retry_wait_max_ms": tftypes.NewValue(tftypes.Number, big.NewFloat(maxRetryWaitMs)),
		})
	})
}

// TestConfigure_UnknownRetrySettingsError: unknown retry values must be
// rejected instead of silently replaced with defaults.
func TestConfigure_UnknownRetrySettingsError(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	for _, attr := range []string{"max_retries", "retry_wait_min_ms", "retry_wait_max_ms"} {
		t.Run(attr, func(t *testing.T) {
			resp := runConfigure(t, map[string]tftypes.Value{
				"token": tftypes.NewValue(tftypes.String, "tok"),
				attr:    tftypes.NewValue(tftypes.Number, tftypes.UnknownValue),
			})
			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected an error for unknown %s", attr)
			}
		})
	}
}

// TestConfigure_RejectsJobTokens: a CI job token goes out as Private-Token,
// which GitLab does not accept for one, so every request would fail as a
// 401 or a 404 that points elsewhere; Configure refuses it up front, by its
// prefix (after an instance prefix too) or by being the job's CI_JOB_TOKEN.
func TestConfigure_RejectsJobTokens(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "")
	cases := []struct {
		name, token, jobToken string
		rejected              bool
	}{
		{name: "prefixed job token", token: "glcbt-eyJhbGciOi.payload.sig", rejected: true},
		{name: "instance prefix", token: "mycorp-glcbt-eyJhbGciOi.payload.sig", rejected: true},
		{name: "the job's CI_JOB_TOKEN", token: "0123456789abcdef0123", jobToken: "0123456789abcdef0123", rejected: true},
		{name: "personal access token", token: "glpat-0123456789abcdefghij"},
		{name: "instance-prefixed personal access token", token: "mycorp-glpat-0123456789abcdefghij"},
		{name: "glcbt inside another token", token: "glpat-abcglcbt-0123456789"},
		{name: "prefix longer than GitLab allows", token: strings.Repeat("a", 21) + "-glcbt-0123456789"},
		{name: "CI_JOB_TOKEN set to another value", token: "glpat-0123456789abcdefghij", jobToken: "0123456789abcdef0123"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CI_JOB_TOKEN", c.jobToken)
			t.Setenv("GITLAB_TOKEN", c.token)
			resp := runConfigure(t, nil)
			if !c.rejected {
				if resp.Diagnostics.HasError() {
					t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
				}
				return
			}
			errs := resp.Diagnostics.Errors()
			if len(errs) != 1 || errs[0].Summary() != "CI job tokens are not supported" {
				t.Fatalf("want the job-token error, got %v", resp.Diagnostics)
			}
			if d, ok := errs[0].(interface{ Path() path.Path }); !ok || !d.Path().Equal(path.Root("token")) {
				t.Errorf("the error must point at the token attribute, got %v", errs[0])
			}
			if !strings.Contains(errs[0].Detail(), "Private-Token") {
				t.Errorf("detail must say why: %s", errs[0].Detail())
			}
		})
	}
}

// TestConfigure_BaseURLCredentialsStayOutOfLogs: a base_url may carry a
// basic-auth proxy's credentials (net/http sends them on every request).
// Neither the configure log line nor the invalid-URL diagnostic may repeat
// them.
func TestConfigure_BaseURLCredentialsStayOutOfLogs(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_BASE_URL", "")
	t.Run("log", func(t *testing.T) {
		var out bytes.Buffer
		resp := runConfigureCtx(tflogtest.RootLogger(t.Context(), &out), t, map[string]tftypes.Value{
			"token":    tftypes.NewValue(tftypes.String, "tok"),
			"base_url": tftypes.NewValue(tftypes.String, "https://ci:s3cret@gitlab.example.com"),
		})
		if resp.Diagnostics.HasError() {
			t.Fatalf("configure: %v", resp.Diagnostics.Errors())
		}
		entries, err := tflogtest.MultilineJSONDecode(&out)
		if err != nil {
			t.Fatalf("decoding the log: %v", err)
		}
		var logged []any
		for _, e := range entries {
			if e["@message"] == "GitLab Commits provider configured" {
				logged = append(logged, e["base_url"])
			}
		}
		if len(logged) != 1 || logged[0] != "https://xxxxx@gitlab.example.com" {
			t.Errorf("logged base_url = %v, want the host with the userinfo masked", logged)
		}
		if strings.Contains(out.String(), "s3cret") || strings.Contains(out.String(), "ci:") {
			t.Errorf("the log carries the credentials: %s", out.String())
		}
	})
	for _, raw := range []string{"htps://ci:s3cret@gitlab.example.com", "ci:s3cret@gitlab.example.com", "https://ci:s3%zz@gitlab.example.com"} {
		t.Run("diagnostic "+raw, func(t *testing.T) {
			resp := runConfigure(t, map[string]tftypes.Value{
				"token":    tftypes.NewValue(tftypes.String, "tok"),
				"base_url": tftypes.NewValue(tftypes.String, raw),
			})
			errs := resp.Diagnostics.Errors()
			if len(errs) != 1 || errs[0].Summary() != "Invalid GitLab base URL" {
				t.Fatalf("want the invalid-URL error, got %v", resp.Diagnostics)
			}
			if d := errs[0].Detail(); strings.Contains(d, "s3") || !strings.Contains(d, "from base_url") {
				t.Errorf("detail must name the source and not the value: %s", d)
			}
		})
	}
	t.Run("diagnostic from GITLAB_BASE_URL", func(t *testing.T) {
		t.Setenv("GITLAB_BASE_URL", "ci:s3cret@gitlab.example.com")
		resp := runConfigure(t, map[string]tftypes.Value{"token": tftypes.NewValue(tftypes.String, "tok")})
		if errs := resp.Diagnostics.Errors(); len(errs) != 1 || !strings.Contains(errs[0].Detail(), "from GITLAB_BASE_URL") {
			t.Fatalf("want the invalid-URL error naming GITLAB_BASE_URL, got %v", resp.Diagnostics)
		}
	})
}

// TestConfigure_RateLimitHeaderIsRaceFree: GitLab.com sends RateLimit-Limit,
// and the provider's requests run concurrently. client-go's own limiter
// reassigns a field on the first response that every later request reads
// unsynchronised, so a request starting in another goroutine after that
// response is a data race; go test -race (which just test runs) reports it
// unless Configure installs its own limiter. That limiter must still take
// its rate from the header.
func TestConfigure_RateLimitHeaderIsRaceFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("RateLimit-Limit", "600000")
		branchJSON(w, "main", "head")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITLAB_TOKEN", "")
	client := configuredClient(t, map[string]tftypes.Value{
		"token":    tftypes.NewValue(tftypes.String, "tok"),
		"base_url": tftypes.NewValue(tftypes.String, srv.URL),
	})

	errs := make(chan error, 2)
	get := func() {
		_, _, err := client.Branches.GetBranch("proj", "main", gitlab.WithContext(t.Context()))
		errs <- err
	}
	go get()
	// Nothing orders the second request after the first: like another
	// resource's refresh, it only starts later. Any synchronisation with the
	// first goroutine would hide the race from the detector.
	go func() {
		time.Sleep(100 * time.Millisecond)
		get()
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("request: %v", err)
		}
	}

	observer, ok := client.HTTPClient().Transport.(*rateLimitObserver)
	if !ok {
		t.Fatalf("transport is %T, want *rateLimitObserver", client.HTTPClient().Transport)
	}
	if got := observer.limiter.current.Load().Limit(); got != rate.Limit(600000.0/60*0.66) {
		t.Errorf("limit = %v, want two thirds of the header's per-second rate", got)
	}
}

// TestHeaderRateLimiter_Observe pins the derivation client-go uses: two
// thirds of the per-minute limit per second as the rate, a third (at least
// one) as the burst, the request that brought the header counted, and only
// the first response considered. counted is checked only where the rate is
// slow enough for the missing token to still show.
func TestHeaderRateLimiter_Observe(t *testing.T) {
	cases := []struct {
		name, header string
		limit        rate.Limit
		burst        int
		counted      bool
	}{
		{name: "gitlab.com style", header: "600", limit: rate.Limit(600.0 / 60 * 0.66), burst: 3, counted: true},
		{name: "burst at least one", header: "60", limit: rate.Limit(60.0 / 60 * 0.66), burst: 1, counted: true},
		{name: "huge value keeps a positive burst", header: "1e300", limit: rate.Limit(1e300 / 60 * 0.66), burst: math.MaxInt32},
		{name: "absent", limit: rate.Inf},
		{name: "zero", header: "0", limit: rate.Inf},
		{name: "negative", header: "-5", limit: rate.Inf},
		{name: "garbage", header: "many", limit: rate.Inf},
		{name: "not a number", header: "NaN", limit: rate.Inf},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newHeaderRateLimiter()
			h := http.Header{}
			if c.header != "" {
				h.Set("RateLimit-Limit", c.header)
			}
			l.observe(h)
			l.observe(http.Header{"Ratelimit-Limit": []string{"6"}})
			lim := l.current.Load()
			if c.limit == rate.Inf && lim.Limit() != rate.Inf ||
				math.Abs(float64(lim.Limit()-c.limit)) > 1e-9*float64(c.limit) {
				t.Errorf("limit = %v, want %v", lim.Limit(), c.limit)
			}
			if c.limit == rate.Inf {
				if err := l.Wait(t.Context()); err != nil {
					t.Errorf("an unset limiter must not block: %v", err)
				}
				return
			}
			if lim.Burst() != c.burst {
				t.Errorf("burst = %d, want %d", lim.Burst(), c.burst)
			}
			if tokens := lim.Tokens(); c.counted && tokens > float64(c.burst)-0.5 {
				t.Errorf("tokens = %v, want the request that brought the header counted", tokens)
			}
		})
	}
}
