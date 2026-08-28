// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"maps"
	"math/big"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// runConfigure invokes the provider's Configure with the given attribute
// overrides (any attribute not supplied defaults to null).
func runConfigure(t *testing.T, attrs map[string]tftypes.Value) *provider.ConfigureResponse {
	t.Helper()
	ctx := t.Context()
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
// and asserts the cross-host redirect guard (P1.3) is installed on the client.
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
	}
	for name, attrs := range cases {
		t.Run(name, func(t *testing.T) {
			if resp := runConfigure(t, attrs); !resp.Diagnostics.HasError() {
				t.Fatal("expected an error diagnostic")
			}
		})
	}
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
