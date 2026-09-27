// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// protocolServer serves the provider the way Terraform talks to it, so tests
// see the framework's own validation and planning of the resource schema.
func protocolServer(t *testing.T) tfprotov6.ProviderServer {
	t.Helper()
	srv, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatalf("provider server: %v", err)
	}
	return srv
}

// filesValue encodes a model as a gitlabcommits_files protocol value.
func filesValue(t *testing.T, m filesResourceModel) *tfprotov6.DynamicValue {
	t.Helper()
	ctx := t.Context()
	sresp := &resource.SchemaResponse{}
	(&filesResource{}).Schema(ctx, resource.SchemaRequest{}, sresp)
	st := tfsdk.State{Schema: sresp.Schema}
	if d := st.Set(ctx, &m); d.HasError() {
		t.Fatalf("state.Set: %v", d)
	}
	dv, err := tfprotov6.NewDynamicValue(sresp.Schema.Type().TerraformType(ctx), st.Raw)
	if err != nil {
		t.Fatalf("NewDynamicValue: %v", err)
	}
	return &dv
}

// configOf is m as configuration: the Computed-only attributes are null.
func configOf(m filesResourceModel) filesResourceModel {
	m.ID = types.StringNull()
	m.CommitSHA = types.StringNull()
	files := make(map[string]fileModel, len(m.Files))
	for p, f := range m.Files {
		f.BlobID = types.StringNull()
		f.LastCommitID = types.StringNull()
		files[p] = f
	}
	m.Files = files
	return m
}

func fileAttr(p, name string) *tftypes.AttributePath {
	return tftypes.NewAttributePath().WithAttributeName("files").WithElementKeyString(p).WithAttributeName(name)
}

// TestFilesSchema_ValidateResourceConfig pins the validators wired into the
// resource schema. An empty files map matters most: Update has no guard of
// its own and would turn it into one commit deleting every managed file.
func TestFilesSchema_ValidateResourceConfig(t *testing.T) {
	withFile := func(f fileModel) filesResourceModel {
		m := configOf(readState(""))
		m.Files["f.txt"] = f
		return m
	}
	cases := []struct {
		wantPath    *tftypes.AttributePath
		name        string
		wantSummary string
		config      filesResourceModel
	}{
		{
			name:   "valid",
			config: configOf(readState("")),
		},
		{
			name: "empty files map",
			config: func() filesResourceModel {
				m := configOf(readState(""))
				m.Files = map[string]fileModel{}
				return m
			}(),
			wantPath:    tftypes.NewAttributePath().WithAttributeName("files"),
			wantSummary: "Empty files map",
		},
		{
			name:        "content conflicts with content_base64",
			config:      withFile(fileModel{Content: types.StringValue("old"), ContentBase64: types.StringValue("b2xk"), ExecuteFilemode: types.BoolValue(false)}),
			wantPath:    fileAttr("f.txt", "content"),
			wantSummary: "Conflicting configuration",
		},
		{
			name:        "content_base64 conflicts with content",
			config:      withFile(fileModel{Content: types.StringValue("old"), ContentBase64: types.StringValue("b2xk"), ExecuteFilemode: types.BoolValue(false)}),
			wantPath:    fileAttr("f.txt", "content_base64"),
			wantSummary: "Conflicting configuration",
		},
		{
			name:        "content_base64 not base64",
			config:      withFile(fileModel{Content: types.StringNull(), ContentBase64: types.StringValue("not base64!"), ExecuteFilemode: types.BoolValue(false)}),
			wantPath:    fileAttr("f.txt", "content_base64"),
			wantSummary: "Invalid base64",
		},
		{
			name:        "neither content nor content_base64",
			config:      withFile(fileModel{Content: types.StringNull(), ContentBase64: types.StringNull(), ExecuteFilemode: types.BoolValue(false)}),
			wantPath:    tftypes.NewAttributePath().WithAttributeName("files").WithElementKeyString("f.txt"),
			wantSummary: "Missing file content",
		},
		{
			name: "traversal in a path",
			config: func() filesResourceModel {
				m := configOf(readState(""))
				m.Files = map[string]fileModel{"a/../b": m.Files["f.txt"]}
				return m
			}(),
			wantPath:    tftypes.NewAttributePath().WithAttributeName("files").WithElementKeyString("a/../b"),
			wantSummary: "Invalid file path",
		},
	}
	srv := protocolServer(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.ValidateResourceConfig(t.Context(), &tfprotov6.ValidateResourceConfigRequest{
				TypeName: "gitlabcommits_files",
				Config:   filesValue(t, tc.config),
			})
			if err != nil {
				t.Fatalf("ValidateResourceConfig: %v", err)
			}
			var errs []*tfprotov6.Diagnostic
			for _, d := range resp.Diagnostics {
				if d.Severity == tfprotov6.DiagnosticSeverityError {
					errs = append(errs, d)
				}
			}
			if tc.wantPath == nil {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			for _, d := range errs {
				if d.Summary == tc.wantSummary && d.Attribute.Equal(tc.wantPath) {
					return
				}
			}
			t.Fatalf("want a %q error at %s, got %v", tc.wantSummary, tc.wantPath, errs)
		})
	}
}

// TestPlannedModel_MatchesFramework ties the CRUD harness to the framework:
// the plan plannedModel builds for Create and Update must be exactly the one
// PlanResourceChange produces. A Computed attribute added to the schema that
// plannedModel does not mark unknown fails here, instead of letting the CRUD
// tests hand the resource a null where Terraform would hand it an unknown.
func TestPlannedModel_MatchesFramework(t *testing.T) {
	ctx := t.Context()
	sresp := &resource.SchemaResponse{}
	(&filesResource{}).Schema(ctx, resource.SchemaRequest{}, sresp)
	typ := sresp.Schema.Type().TerraformType(ctx)

	twoFiles := func() filesResourceModel {
		m := readState("oldblob")
		m.Files["g.txt"] = fileModel{
			Content:         types.StringValue("same"),
			ContentBase64:   types.StringNull(),
			BlobID:          types.StringValue("gblob"),
			LastCommitID:    types.StringValue("glcid"),
			ExecuteFilemode: types.BoolValue(true),
		}
		return m
	}

	cases := []struct {
		prior    func(t *testing.T) *tfprotov6.DynamicValue
		name     string
		proposed filesResourceModel
		create   bool
	}{
		{
			name: "create",
			prior: func(t *testing.T) *tfprotov6.DynamicValue {
				dv, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, nil))
				if err != nil {
					t.Fatalf("NewDynamicValue: %v", err)
				}
				return &dv
			},
			proposed: configOf(twoFiles()),
			create:   true,
		},
		{
			name:  "update",
			prior: func(t *testing.T) *tfprotov6.DynamicValue { return filesValue(t, twoFiles()) },
			proposed: func() filesResourceModel {
				m := twoFiles()
				f := m.Files["f.txt"]
				f.Content = types.StringValue("changed")
				m.Files["f.txt"] = f
				return m
			}(),
		},
	}
	srv := protocolServer(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.PlanResourceChange(t.Context(), &tfprotov6.PlanResourceChangeRequest{
				TypeName:         "gitlabcommits_files",
				PriorState:       tc.prior(t),
				ProposedNewState: filesValue(t, tc.proposed),
				Config:           filesValue(t, configOf(tc.proposed)),
			})
			if err != nil {
				t.Fatalf("PlanResourceChange: %v", err)
			}
			for _, d := range resp.Diagnostics {
				if d.Severity == tfprotov6.DiagnosticSeverityError {
					t.Fatalf("unexpected plan error: %s: %s", d.Summary, d.Detail)
				}
			}
			got, err := resp.PlannedState.Unmarshal(typ)
			if err != nil {
				t.Fatalf("PlannedState.Unmarshal: %v", err)
			}
			want, err := filesValue(t, plannedModel(tc.proposed, tc.create)).Unmarshal(typ)
			if err != nil {
				t.Fatalf("plannedModel.Unmarshal: %v", err)
			}
			if !got.Equal(want) {
				diffs, _ := got.Diff(want)
				t.Fatalf("plannedModel differs from the framework's plan: %v", diffs)
			}
		})
	}
}

// TestFilesSchema_ProjectAndBranchRequireReplace: project_id and branch
// select the repository location, so changing either must replace the
// resource. As an in-place Update, diffActions compares only files and would
// record the files on a branch that never received them.
func TestFilesSchema_ProjectAndBranchRequireReplace(t *testing.T) {
	cases := []struct {
		change      func(*filesResourceModel)
		wantReplace *tftypes.AttributePath
		name        string
	}{
		{name: "project_id", change: func(m *filesResourceModel) { m.ProjectID = types.StringValue("other") },
			wantReplace: tftypes.NewAttributePath().WithAttributeName("project_id")},
		{name: "branch", change: func(m *filesResourceModel) { m.Branch = types.StringValue("other") },
			wantReplace: tftypes.NewAttributePath().WithAttributeName("branch")},
		{name: "content only", change: func(m *filesResourceModel) {
			f := m.Files["f.txt"]
			f.Content = types.StringValue("changed")
			m.Files["f.txt"] = f
		}},
	}
	srv := protocolServer(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proposed := readState("oldblob")
			tc.change(&proposed)
			resp, err := srv.PlanResourceChange(t.Context(), &tfprotov6.PlanResourceChangeRequest{
				TypeName:         "gitlabcommits_files",
				PriorState:       filesValue(t, readState("oldblob")),
				ProposedNewState: filesValue(t, proposed),
				Config:           filesValue(t, configOf(proposed)),
			})
			if err != nil {
				t.Fatalf("PlanResourceChange: %v", err)
			}
			for _, d := range resp.Diagnostics {
				if d.Severity == tfprotov6.DiagnosticSeverityError {
					t.Fatalf("unexpected plan error: %s: %s", d.Summary, d.Detail)
				}
			}
			if tc.wantReplace == nil {
				if len(resp.RequiresReplace) != 0 {
					t.Fatalf("a content change must update in place, got replace on %v", resp.RequiresReplace)
				}
				return
			}
			if len(resp.RequiresReplace) != 1 || !resp.RequiresReplace[0].Equal(tc.wantReplace) {
				t.Fatalf("RequiresReplace = %v, want [%s]", resp.RequiresReplace, tc.wantReplace)
			}
		})
	}
}
