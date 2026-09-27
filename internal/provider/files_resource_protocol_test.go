// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"maps"
	"slices"
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
		null        *tftypes.AttributePath
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
			name: "null entry",
			config: func() filesResourceModel {
				m := configOf(readState(""))
				m.Files["g.txt"] = m.Files["f.txt"]
				return m
			}(),
			null:        tftypes.NewAttributePath().WithAttributeName("files").WithElementKeyString("g.txt"),
			wantPath:    tftypes.NewAttributePath().WithAttributeName("files").WithElementKeyString("g.txt"),
			wantSummary: "Null file entry",
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
			config := filesValue(t, tc.config)
			if tc.null != nil {
				config = replacedAt(t, config, tc.null, false)
			}
			resp, err := srv.ValidateResourceConfig(t.Context(), &tfprotov6.ValidateResourceConfigRequest{
				TypeName: "gitlabcommits_files",
				Config:   config,
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
// plannedModel does not mark unknown, or a change to what ModifyPlan keeps,
// fails here, instead of letting the CRUD tests hand the resource a value
// Terraform would never hand it.
func TestPlannedModel_MatchesFramework(t *testing.T) {
	ctx := t.Context()
	sresp := &resource.SchemaResponse{}
	(&filesResource{}).Schema(ctx, resource.SchemaRequest{}, sresp)
	typ := sresp.Schema.Type().TerraformType(ctx)

	twoFiles := func() filesResourceModel {
		m := readState("oldblob")
		m.Files["g.txt"] = fileModel{
			Content:         types.StringNull(),
			ContentBase64:   types.StringValue("c2FtZQ=="),
			BlobID:          types.StringValue("gblob"),
			LastCommitID:    types.StringValue("glcid"),
			ExecuteFilemode: types.BoolValue(true),
		}
		return m
	}
	edit := func(change func(m *filesResourceModel)) filesResourceModel {
		m := twoFiles()
		change(&m)
		return m
	}

	cases := []struct {
		name     string
		proposed filesResourceModel
		create   bool
	}{
		{
			name:     "create",
			proposed: configOf(twoFiles()),
			create:   true,
		},
		{
			name: "update",
			proposed: edit(func(m *filesResourceModel) {
				f := m.Files["f.txt"]
				f.Content = types.StringValue("changed")
				m.Files["f.txt"] = f
			}),
		},
		{
			name: "content_base64 edited",
			proposed: edit(func(m *filesResourceModel) {
				f := m.Files["g.txt"]
				f.ContentBase64 = types.StringValue("b3RoZXI=")
				m.Files["g.txt"] = f
			}),
		},
		{
			name:     "commit_message only",
			proposed: edit(func(m *filesResourceModel) { m.CommitMessage = types.StringValue("other") }),
		},
		{
			name: "file added",
			proposed: edit(func(m *filesResourceModel) {
				m.Files["h.txt"] = fileModel{Content: types.StringValue("new"), ContentBase64: types.StringNull(),
					BlobID: types.StringNull(), LastCommitID: types.StringNull(), ExecuteFilemode: types.BoolValue(false)}
			}),
		},
		{
			name:     "file removed",
			proposed: edit(func(m *filesResourceModel) { delete(m.Files, "g.txt") }),
		},
		{
			name: "execute_filemode flipped",
			proposed: edit(func(m *filesResourceModel) {
				f := m.Files["g.txt"]
				f.ExecuteFilemode = types.BoolValue(false)
				m.Files["g.txt"] = f
			}),
		},
		{
			name:     "branch changed",
			proposed: edit(func(m *filesResourceModel) { m.Branch = types.StringValue("other") }),
		},
	}
	srv := protocolServer(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var prior *filesResourceModel
			priorValue, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, nil))
			if err != nil {
				t.Fatalf("NewDynamicValue: %v", err)
			}
			if !tc.create {
				m := twoFiles()
				prior, priorValue = &m, *filesValue(t, m)
			}
			resp, err := srv.PlanResourceChange(t.Context(), &tfprotov6.PlanResourceChangeRequest{
				TypeName:         "gitlabcommits_files",
				PriorState:       &priorValue,
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
			want, err := filesValue(t, plannedModel(tc.proposed, prior)).Unmarshal(typ)
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
// record the files on a branch that never received them. A value unknown at
// plan time counts as a change, as the attribute descriptions say: the plan
// cannot promise an in-place update that may turn into a replacement.
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
		{name: "project_id unknown", change: func(m *filesResourceModel) { m.ProjectID = types.StringUnknown() },
			wantReplace: tftypes.NewAttributePath().WithAttributeName("project_id")},
		{name: "branch unknown", change: func(m *filesResourceModel) { m.Branch = types.StringUnknown() },
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

// replacedAt returns dv with the value at p made null (unknown false) or
// unknown (unknown true), for values filesValue cannot encode.
func replacedAt(t *testing.T, dv *tfprotov6.DynamicValue, p *tftypes.AttributePath, unknown bool) *tfprotov6.DynamicValue {
	t.Helper()
	ctx := t.Context()
	sresp := &resource.SchemaResponse{}
	(&filesResource{}).Schema(ctx, resource.SchemaRequest{}, sresp)
	typ := sresp.Schema.Type().TerraformType(ctx)
	raw, err := dv.Unmarshal(typ)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	found := false
	raw, err = tftypes.Transform(raw, func(at *tftypes.AttributePath, v tftypes.Value) (tftypes.Value, error) {
		if !at.Equal(p) {
			return v, nil
		}
		found = true
		if unknown {
			return tftypes.NewValue(v.Type(), tftypes.UnknownValue), nil
		}
		return tftypes.NewValue(v.Type(), nil), nil
	})
	if err != nil || !found {
		t.Fatalf("replacing %s: found=%v err=%v", p, found, err)
	}
	out, err := tfprotov6.NewDynamicValue(typ, raw)
	if err != nil {
		t.Fatalf("NewDynamicValue: %v", err)
	}
	return &out
}

// valueAt returns the value at p in v.
func valueAt(t *testing.T, v tftypes.Value, p *tftypes.AttributePath) tftypes.Value {
	t.Helper()
	got, _, err := tftypes.WalkAttributePath(v, p)
	if err != nil {
		t.Fatalf("walking %s: %v", p, err)
	}
	return got.(tftypes.Value)
}

// TestModifyPlan_KeepsWhatApplyKeeps: the plan keeps the blob_id and
// last_commit_id of every file whose content, content_base64 and
// execute_filemode stay as state has them, and commit_sha when that holds
// for every file and none is added or removed, since Update carries exactly
// those values over (TestUpdate_AppliedStateKeepsThePlan). Everything else
// stays known after apply: a known value Update then changed would fail the
// apply as an inconsistent result, with the commit already landed.
func TestModifyPlan_KeepsWhatApplyKeeps(t *testing.T) {
	ctx := t.Context()
	sresp := &resource.SchemaResponse{}
	(&filesResource{}).Schema(ctx, resource.SchemaRequest{}, sresp)
	typ := sresp.Schema.Type().TerraformType(ctx)

	threeFiles := func() filesResourceModel {
		m := readState("")
		m.CommitSHA = types.StringValue("sha01")
		m.Files = map[string]fileModel{}
		for _, p := range []string{"d/a.yaml", "d/b.yaml", "d/c.yaml"} {
			m.Files[p] = fileModel{
				Content: types.StringValue("x"), ContentBase64: types.StringNull(), ExecuteFilemode: types.BoolValue(false),
				BlobID: types.StringValue("blob-" + p), LastCommitID: types.StringValue("lcid-" + p),
			}
		}
		return m
	}
	imported := func() filesResourceModel {
		m := readState("")
		m.CommitSHA = types.StringNull()
		m.Files = nil
		return m
	}
	setFile := func(p string, change func(f *fileModel)) func(m *filesResourceModel) {
		return func(m *filesResourceModel) {
			f := m.Files[p]
			change(&f)
			m.Files[p] = f
		}
	}
	all := []string{"d/a.yaml", "d/b.yaml", "d/c.yaml"}

	cases := []struct {
		prior   func() filesResourceModel
		change  func(m *filesResourceModel)
		unknown *tftypes.AttributePath
		name    string
		kept    []string
		keptSHA bool
	}{
		{name: "no change", kept: all, keptSHA: true},
		{name: "one file edited", change: setFile("d/a.yaml", func(f *fileModel) { f.Content = types.StringValue("y") }),
			kept: []string{"d/b.yaml", "d/c.yaml"}},
		{name: "commit_message only", change: func(m *filesResourceModel) { m.CommitMessage = types.StringValue("other") },
			kept: all, keptSHA: true},
		{name: "optimistic_lock only", change: func(m *filesResourceModel) { m.OptimisticLock = types.BoolValue(false) },
			kept: all, keptSHA: true},
		{name: "commit_message unknown", unknown: tftypes.NewAttributePath().WithAttributeName("commit_message"),
			kept: all, keptSHA: true},
		{name: "execute_filemode flipped", change: setFile("d/b.yaml", func(f *fileModel) { f.ExecuteFilemode = types.BoolValue(true) }),
			kept: []string{"d/a.yaml", "d/c.yaml"}},
		{name: "file added", change: func(m *filesResourceModel) {
			m.Files["d/n.yaml"] = fileModel{Content: types.StringValue("x"), ContentBase64: types.StringNull(),
				BlobID: types.StringNull(), LastCommitID: types.StringNull(), ExecuteFilemode: types.BoolValue(false)}
		}, kept: all},
		{name: "file removed", change: func(m *filesResourceModel) { delete(m.Files, "d/c.yaml") },
			kept: []string{"d/a.yaml", "d/b.yaml"}},
		{name: "switched to content_base64 of the same bytes", change: setFile("d/a.yaml", func(f *fileModel) {
			f.Content, f.ContentBase64 = types.StringNull(), types.StringValue("eA==")
		}), kept: []string{"d/b.yaml", "d/c.yaml"}},
		{name: "content_base64 edited", prior: func() filesResourceModel {
			m := threeFiles()
			setFile("d/a.yaml", func(f *fileModel) { f.Content, f.ContentBase64 = types.StringNull(), types.StringValue("eA==") })(&m)
			return m
		}, change: setFile("d/a.yaml", func(f *fileModel) { f.ContentBase64 = types.StringValue("eQ==") }),
			kept: []string{"d/b.yaml", "d/c.yaml"}},
		{name: "content unknown", unknown: fileAttr("d/a.yaml", "content"), kept: []string{"d/b.yaml", "d/c.yaml"}},
		{name: "execute_filemode unknown", unknown: fileAttr("d/c.yaml", "execute_filemode"), kept: []string{"d/a.yaml", "d/b.yaml"}},
		{name: "files unknown", unknown: tftypes.NewAttributePath().WithAttributeName("files")},
		{name: "files unknown after import", prior: imported, change: func(m *filesResourceModel) { m.Files = threeFiles().Files },
			unknown: tftypes.NewAttributePath().WithAttributeName("files")},
		{name: "files added after import", prior: imported, change: func(m *filesResourceModel) { m.Files = threeFiles().Files }},
		{name: "project_id changed", change: func(m *filesResourceModel) { m.ProjectID = types.StringValue("other") }},
		{name: "branch unknown", unknown: tftypes.NewAttributePath().WithAttributeName("branch")},
	}
	srv := protocolServer(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.prior == nil {
				tc.prior = threeFiles
			}
			prior := tc.prior()
			proposed := tc.prior()
			if tc.change != nil {
				tc.change(&proposed)
			}
			proposedValue, configValue := filesValue(t, proposed), filesValue(t, configOf(proposed))
			if tc.unknown != nil {
				proposedValue = replacedAt(t, proposedValue, tc.unknown, true)
				configValue = replacedAt(t, configValue, tc.unknown, true)
			}
			priorValue := filesValue(t, prior)
			resp, err := srv.PlanResourceChange(t.Context(), &tfprotov6.PlanResourceChangeRequest{
				TypeName:         "gitlabcommits_files",
				PriorState:       priorValue,
				ProposedNewState: proposedValue,
				Config:           configValue,
			})
			if err != nil {
				t.Fatalf("PlanResourceChange: %v", err)
			}
			for _, d := range resp.Diagnostics {
				if d.Severity == tfprotov6.DiagnosticSeverityError {
					t.Fatalf("unexpected plan error: %s: %s", d.Summary, d.Detail)
				}
			}
			planned, err := resp.PlannedState.Unmarshal(typ)
			if err != nil {
				t.Fatalf("PlannedState.Unmarshal: %v", err)
			}
			priorRaw, err := priorValue.Unmarshal(typ)
			if err != nil {
				t.Fatalf("prior Unmarshal: %v", err)
			}

			var files map[string]tftypes.Value
			if plannedFiles := valueAt(t, planned, tftypes.NewAttributePath().WithAttributeName("files")); plannedFiles.IsKnown() {
				if err := plannedFiles.As(&files); err != nil {
					t.Fatalf("planned files: %v", err)
				}
			}
			for _, p := range sortedKeys(files) {
				for _, name := range []string{"blob_id", "last_commit_id"} {
					got := valueAt(t, planned, fileAttr(p, name))
					if !slices.Contains(tc.kept, p) {
						if got.IsKnown() {
							t.Errorf("%s of %q is planned as %s, want known after apply", name, p, got)
						}
						continue
					}
					if want := valueAt(t, priorRaw, fileAttr(p, name)); !got.Equal(want) {
						t.Errorf("%s of %q is planned as %s, want the prior %s", name, p, got, want)
					}
				}
			}
			for _, p := range tc.kept {
				if _, ok := files[p]; !ok {
					t.Errorf("%q is missing from the planned files", p)
				}
			}
			sha := tftypes.NewAttributePath().WithAttributeName("commit_sha")
			got := valueAt(t, planned, sha)
			switch want := valueAt(t, priorRaw, sha); {
			case tc.keptSHA && !got.Equal(want):
				t.Errorf("commit_sha is planned as %s, want the prior %s", got, want)
			case !tc.keptSHA && got.IsKnown():
				t.Errorf("commit_sha is planned as %s, want known after apply", got)
			}
		})
	}

	t.Run("destroy", func(t *testing.T) {
		null, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, nil))
		if err != nil {
			t.Fatalf("NewDynamicValue: %v", err)
		}
		resp, err := srv.PlanResourceChange(t.Context(), &tfprotov6.PlanResourceChangeRequest{
			TypeName:         "gitlabcommits_files",
			PriorState:       filesValue(t, threeFiles()),
			ProposedNewState: &null,
			Config:           &null,
		})
		if err != nil {
			t.Fatalf("PlanResourceChange: %v", err)
		}
		if len(resp.Diagnostics) != 0 {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		planned, err := resp.PlannedState.Unmarshal(typ)
		if err != nil {
			t.Fatalf("PlannedState.Unmarshal: %v", err)
		}
		if !planned.IsNull() {
			t.Fatalf("a destroy must plan a null object, got %s", planned)
		}
	})
}

// TestUpdate_AppliedStateKeepsThePlan drives Update with the plan the
// framework produces, ModifyPlan included, and requires the applied state to
// keep every value that plan knew (checkUpdateResult): an untouched file
// beside an edit, a commit_message change that commits nothing, and the
// deletes Update drops after planning, for a path another resource claimed
// and for a token-less delete of a path already gone, as well as an
// identical adoption. Terraform would fail each of those applies otherwise.
func TestUpdate_AppliedStateKeepsThePlan(t *testing.T) {
	ctx := t.Context()
	sresp := &resource.SchemaResponse{}
	(&filesResource{}).Schema(ctx, resource.SchemaRequest{}, sresp)
	sch := sresp.Schema
	typ := sch.Type().TerraformType(ctx)

	state := managedFiles(true, map[string]string{"a.txt": "l1", "b.txt": "l1", "c.txt": "l1"})
	for p, f := range state.Files {
		f.BlobID = types.StringValue("blob-l1")
		state.Files[p] = f
	}
	state.CommitSHA = types.StringValue("l1")
	edit := func(change func(m *filesResourceModel)) filesResourceModel {
		m := state
		m.Files = maps.Clone(state.Files)
		change(&m)
		return m
	}

	cases := []struct {
		repo        map[string]string
		claimed     []string
		name        string
		plan        filesResourceModel
		wantCommits [][]string
		shaKnown    bool
	}{
		{
			name:        "an edit beside untouched files",
			repo:        map[string]string{"a.txt": "l1", "b.txt": "l1", "c.txt": "l1"},
			plan:        withContent(edit(func(*filesResourceModel) {}), map[string]string{"a.txt": "y"}),
			wantCommits: [][]string{{"update:a.txt@l1"}},
		},
		{
			name:     "commit_message only",
			repo:     map[string]string{"a.txt": "l1", "b.txt": "l1", "c.txt": "l1"},
			plan:     edit(func(m *filesResourceModel) { m.CommitMessage = types.StringValue("other") }),
			shaKnown: true,
		},
		{
			name:    "a delete of a path another resource claimed",
			repo:    map[string]string{"a.txt": "l1", "b.txt": "l1", "c.txt": "l1"},
			claimed: []string{"c.txt"},
			plan:    edit(func(m *filesResourceModel) { delete(m.Files, "c.txt") }),
		},
		{
			name: "a token-less delete of a path already gone",
			repo: map[string]string{"a.txt": "l1", "b.txt": "l1"},
			plan: edit(func(m *filesResourceModel) {
				m.OptimisticLock = types.BoolValue(false)
				delete(m.Files, "c.txt")
			}),
		},
		{
			name: "an identical adoption",
			repo: map[string]string{"a.txt": "l1", "b.txt": "l1", "c.txt": "l1", "d.txt": "l0"},
			plan: edit(func(m *filesResourceModel) { m.Files["d.txt"] = state.Files["a.txt"] }),
		},
	}
	srv := protocolServer(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planResp, err := srv.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
				TypeName:         "gitlabcommits_files",
				PriorState:       filesValue(t, state),
				ProposedNewState: filesValue(t, tc.plan),
				Config:           filesValue(t, configOf(tc.plan)),
			})
			if err != nil || len(planResp.Diagnostics) != 0 {
				t.Fatalf("PlanResourceChange: %v %v", err, planResp.Diagnostics)
			}
			planned, err := planResp.PlannedState.Unmarshal(typ)
			if err != nil {
				t.Fatalf("PlannedState.Unmarshal: %v", err)
			}
			if !valueAt(t, planned, fileAttr("b.txt", "blob_id")).IsKnown() {
				t.Fatalf("the untouched b.txt must be planned with its blob_id known: %s", planned)
			}
			if got := valueAt(t, planned, tftypes.NewAttributePath().WithAttributeName("commit_sha")).IsKnown(); got != tc.shaKnown {
				t.Fatalf("commit_sha planned known = %v, want %v", got, tc.shaKnown)
			}

			fake := &repoFake{files: maps.Clone(tc.repo)}
			res := newTestResource(fake.client(t))
			res.locks.claim("proj", "main", tc.claimed)
			st := tfsdk.State{Schema: sch}
			if d := st.Set(ctx, &state); d.HasError() {
				t.Fatalf("state.Set: %v", d)
			}
			req := resource.UpdateRequest{Plan: tfsdk.Plan{Schema: sch, Raw: planned}, State: st}
			resp := &resource.UpdateResponse{State: tfsdk.State{Schema: sch, Raw: st.Raw.Copy()}}
			res.Update(ctx, req, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			checkUpdateResult(t, req, resp)
			if commits, _, _ := fake.recorded(); !slices.EqualFunc(commits, tc.wantCommits, slices.Equal) {
				t.Errorf("commits = %q, want %q", commits, tc.wantCommits)
			}
		})
	}
}
