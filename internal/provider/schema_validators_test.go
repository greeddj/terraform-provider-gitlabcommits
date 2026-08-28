// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// TestStringConflictsWithSibling exercises the content/content_base64 mutual
// exclusion directly: setting both is a conflict, setting one is fine. A
// synthetic flat config stands in for the nested file object, so no
// resource.Test machinery is needed.
func TestStringConflictsWithSibling(t *testing.T) {
	ctx := t.Context()
	sch := rschema.Schema{Attributes: map[string]rschema.Attribute{
		"content":        rschema.StringAttribute{Optional: true},
		"content_base64": rschema.StringAttribute{Optional: true},
	}}
	ptr := func(s string) *string { return &s }
	cfg := func(content, b64 *string) tfsdk.Config {
		toVal := func(s *string) tftypes.Value {
			if s == nil {
				return tftypes.NewValue(tftypes.String, nil)
			}
			return tftypes.NewValue(tftypes.String, *s)
		}
		raw := tftypes.NewValue(sch.Type().TerraformType(ctx), map[string]tftypes.Value{
			"content":        toVal(content),
			"content_base64": toVal(b64),
		})
		return tfsdk.Config{Schema: sch, Raw: raw}
	}

	t.Run("both set conflicts", func(t *testing.T) {
		resp := &validator.StringResponse{}
		stringConflictsWithSibling("content_base64").ValidateString(ctx, validator.StringRequest{
			Path:        path.Root("content"),
			ConfigValue: types.StringValue("x"),
			Config:      cfg(ptr("x"), ptr("eA==")),
		}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected a conflict error when both content and content_base64 are set")
		}
	})

	t.Run("only content is fine", func(t *testing.T) {
		resp := &validator.StringResponse{}
		stringConflictsWithSibling("content_base64").ValidateString(ctx, validator.StringRequest{
			Path:        path.Root("content"),
			ConfigValue: types.StringValue("x"),
			Config:      cfg(ptr("x"), nil),
		}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
		}
	})
}

// TestObjectFileContentRequired pins TF-1: a file object with neither content
// nor content_base64 is rejected at validate time; setting either one passes;
// an unknown value defers rather than false-positives.
func TestObjectFileContentRequired(t *testing.T) {
	attrTypes := map[string]attr.Type{
		"content":        types.StringType,
		"content_base64": types.StringType,
	}
	mk := func(content, b64 types.String) types.Object {
		o, d := types.ObjectValue(attrTypes, map[string]attr.Value{
			"content":        content,
			"content_base64": b64,
		})
		if d.HasError() {
			t.Fatalf("ObjectValue: %v", d)
		}
		return o
	}

	cases := []struct {
		name    string
		obj     types.Object
		wantErr bool
	}{
		{"both null", mk(types.StringNull(), types.StringNull()), true},
		{"content set", mk(types.StringValue("x"), types.StringNull()), false},
		{"base64 set", mk(types.StringNull(), types.StringValue("eA==")), false},
		{"content unknown defers", mk(types.StringUnknown(), types.StringNull()), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &validator.ObjectResponse{}
			objectFileContentRequired().ValidateObject(t.Context(),
				validator.ObjectRequest{Path: path.Root("files").AtMapKey("x"), ConfigValue: c.obj}, resp)
			if c.wantErr != resp.Diagnostics.HasError() {
				t.Fatalf("wantErr=%v, got diags=%v", c.wantErr, resp.Diagnostics)
			}
		})
	}
}

func TestStringNotEmpty(t *testing.T) {
	cases := []struct {
		name    string
		value   types.String
		wantErr bool
	}{
		{"non-empty", types.StringValue("hello"), false},
		{"empty", types.StringValue(""), true},
		{"whitespace-only", types.StringValue("   "), true},
		{"null", types.StringNull(), false},
		{"unknown", types.StringUnknown(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validator.StringRequest{
				ConfigValue: c.value,
				Path:        path.Root("attr"),
			}
			resp := &validator.StringResponse{}
			stringNotEmpty().ValidateString(t.Context(), req, resp)
			if got := resp.Diagnostics.HasError(); got != c.wantErr {
				t.Fatalf("hasError=%v want %v; diags=%v", got, c.wantErr, resp.Diagnostics)
			}
		})
	}
}

func TestStringMatchesRegex(t *testing.T) {
	v := stringMatchesRegex(`^[a-z]+$`, "must be lowercase letters")
	cases := []struct {
		name    string
		value   types.String
		wantErr bool
	}{
		{"matches", types.StringValue("hello"), false},
		{"has-digits", types.StringValue("abc123"), true},
		{"empty-string", types.StringValue(""), true},
		{"null", types.StringNull(), false},
		{"unknown", types.StringUnknown(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validator.StringRequest{
				ConfigValue: c.value,
				Path:        path.Root("attr"),
			}
			resp := &validator.StringResponse{}
			v.ValidateString(t.Context(), req, resp)
			if got := resp.Diagnostics.HasError(); got != c.wantErr {
				t.Fatalf("hasError=%v want %v", got, c.wantErr)
			}
		})
	}
}

func TestStringIsBase64(t *testing.T) {
	cases := []struct {
		name    string
		value   types.String
		wantErr bool
	}{
		{"valid", types.StringValue("aGVsbG8="), false},
		{"empty", types.StringValue(""), false},
		{"invalid", types.StringValue("not-base64!!!"), true},
		{"null", types.StringNull(), false},
		{"unknown", types.StringUnknown(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &validator.StringResponse{}
			stringIsBase64().ValidateString(t.Context(), validator.StringRequest{ConfigValue: c.value, Path: path.Root("content_base64")}, resp)
			if got := resp.Diagnostics.HasError(); got != c.wantErr {
				t.Fatalf("hasError=%v want %v", got, c.wantErr)
			}
		})
	}
}

func TestStringValidRepoPath(t *testing.T) {
	cases := []struct {
		name    string
		value   types.String
		wantErr bool
	}{
		{"plain", types.StringValue("dir/file.yaml"), false},
		{"leading slash", types.StringValue("/abs"), true},
		{"traversal", types.StringValue("a/../b"), true},
		{"null", types.StringNull(), false},
		{"unknown", types.StringUnknown(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &validator.StringResponse{}
			stringValidRepoPath().ValidateString(t.Context(), validator.StringRequest{ConfigValue: c.value, Path: path.Root("file_path")}, resp)
			if got := resp.Diagnostics.HasError(); got != c.wantErr {
				t.Fatalf("hasError=%v want %v", got, c.wantErr)
			}
		})
	}
}

func TestStringBranchName(t *testing.T) {
	cases := map[string]bool{"main": false, "release/1.2": false, "feature_x-1": false, "has space": true, "tab\tname": true}
	for value, wantErr := range cases {
		resp := &validator.StringResponse{}
		stringBranchName().ValidateString(t.Context(), validator.StringRequest{ConfigValue: types.StringValue(value), Path: path.Root("branch")}, resp)
		if got := resp.Diagnostics.HasError(); got != wantErr {
			t.Errorf("%q: hasError=%v want %v", value, got, wantErr)
		}
	}
}

func TestMapNonEmpty(t *testing.T) {
	elemType := types.ObjectType{AttrTypes: map[string]attr.Type{
		"x": types.StringType,
	}}
	nonEmpty := types.MapValueMust(elemType, map[string]attr.Value{
		"k": types.ObjectValueMust(elemType.AttrTypes, map[string]attr.Value{
			"x": types.StringValue("v"),
		}),
	})
	empty := types.MapValueMust(elemType, map[string]attr.Value{})

	cases := []struct {
		value   types.Map
		name    string
		wantErr bool
	}{
		{name: "populated", value: nonEmpty, wantErr: false},
		{name: "empty", value: empty, wantErr: true},
		{name: "null", value: types.MapNull(elemType), wantErr: false},
		{name: "unknown", value: types.MapUnknown(elemType), wantErr: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validator.MapRequest{
				ConfigValue: c.value,
				Path:        path.Root("files"),
			}
			resp := &validator.MapResponse{}
			mapNonEmpty().ValidateMap(t.Context(), req, resp)
			if got := resp.Diagnostics.HasError(); got != c.wantErr {
				t.Fatalf("hasError=%v want %v", got, c.wantErr)
			}
		})
	}
}

func TestMapKeysValidRepoPath(t *testing.T) {
	// Pins the path-validation contract: relative paths only, no traversal
	// segments, no NUL bytes, no empty segments, no segment starting with
	// whitespace - while legal-but-odd git names stay usable.
	v := mapKeysValidRepoPath()
	cases := []struct {
		name    string
		keys    []string
		wantErr bool
	}{
		{"plain", []string{"foo.yaml", "bar/baz.yaml"}, false},
		{"hidden-file", []string{".gitignore", ".github/CODEOWNERS"}, false},
		{"deep-path", []string{"services/frontend/values/dev.yaml"}, false},
		{"double-dot-name", []string{"..config", "a/..b"}, false},
		{"interior-space", []string{"docs/read me.txt"}, false},
		{"dotdot-segment", []string{"foo/../bar"}, true},
		{"leading-slash", []string{"/abs/path"}, true},
		{"single-dot-segment", []string{"./foo"}, true},
		{"nul-byte", []string{"foo\x00bar"}, true},
		{"trailing-slash", []string{"foo/"}, true},
		{"doubled-slash", []string{"a//b"}, true},
		{"leading-space-segment", []string{"a/ b.txt"}, true},
		{"empty-map-ignored", []string{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			elements := make(map[string]attr.Value, len(c.keys))
			for _, k := range c.keys {
				elements[k] = types.StringValue("dummy")
			}
			req := validator.MapRequest{
				ConfigValue: types.MapValueMust(types.StringType, elements),
				Path:        path.Root("files"),
			}
			resp := &validator.MapResponse{}
			v.ValidateMap(t.Context(), req, resp)
			if got := resp.Diagnostics.HasError(); got != c.wantErr {
				t.Fatalf("hasError=%v want %v; diags=%v", got, c.wantErr, resp.Diagnostics)
			}
		})
	}
}
