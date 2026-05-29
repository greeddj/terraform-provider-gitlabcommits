// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
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
	ctx := context.Background()
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
	for k, v := range attrs {
		vals[k] = v
	}
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
	client, ok := resp.ResourceData.(*gitlab.Client)
	if !ok || client == nil {
		t.Fatalf("expected a configured *gitlab.Client, got %T", resp.ResourceData)
	}
	if client.HTTPClient().CheckRedirect == nil {
		t.Error("expected the cross-host redirect guard to be installed on the client")
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
