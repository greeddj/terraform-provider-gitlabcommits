// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// runBranchHeadDataSourceRead drives branchHeadDataSource.Read for the fixed
// config (proj, main) against a faked client and returns the response plus
// the resulting model on success.
func runBranchHeadDataSourceRead(t *testing.T, client *gitlab.Client) (*datasource.ReadResponse, branchHeadModel) {
	t.Helper()
	d := &branchHeadDataSource{client: client}
	ctx := t.Context()

	schemaResp := &datasource.SchemaResponse{}
	d.Schema(ctx, datasource.SchemaRequest{}, schemaResp)
	sch := schemaResp.Schema

	raw := tftypes.NewValue(sch.Type().TerraformType(ctx), map[string]tftypes.Value{
		"project_id": tftypes.NewValue(tftypes.String, "proj"),
		"branch":     tftypes.NewValue(tftypes.String, "main"),
		"commit_sha": tftypes.NewValue(tftypes.String, nil),
		"protected":  tftypes.NewValue(tftypes.Bool, nil),
	})
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: raw}}
	resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sch}}
	d.Read(ctx, req, resp)

	var out branchHeadModel
	if !resp.Diagnostics.HasError() {
		if diags := resp.State.Get(ctx, &out); diags.HasError() {
			t.Fatalf("state.Get: %v", diags)
		}
	}
	return resp, out
}

// TestBranchHeadDataSource_HappyPath: commit_sha and protected come straight
// from the branch response.
func TestBranchHeadDataSource_HappyPath(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"main","protected":true,"commit":{"id":"abc123"}}`))
	})

	resp, out := runBranchHeadDataSourceRead(t, client)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if out.CommitSHA.ValueString() != "abc123" || !out.Protected.ValueBool() {
		t.Errorf("commit_sha/protected = %q/%v, want abc123/true", out.CommitSHA.ValueString(), out.Protected.ValueBool())
	}
}

// TestBranchHeadDataSource_NotFound: a 404 maps to the dedicated diagnostic
// that names the branch. client-go answers every 404 with one shared error
// before it reads the body, so a project the token cannot see looks exactly
// like a missing branch, and the detail must not claim more than that.
func TestBranchHeadDataSource_NotFound(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
	})

	resp, _ := runBranchHeadDataSourceRead(t, client)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for a missing branch")
	}
	d := resp.Diagnostics.Errors()[0]
	if got := d.Summary(); got != "Branch not found" {
		t.Errorf("summary = %q, want %q", got, "Branch not found")
	}
	for _, want := range []string{
		`branch "main" in project "proj" was not found`,
		"the token cannot see the project",
	} {
		if !strings.Contains(d.Detail(), want) {
			t.Errorf("detail %q does not contain %q", d.Detail(), want)
		}
	}
}
