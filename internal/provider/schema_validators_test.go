// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// stringConflictsWithSibling is intentionally not unit-tested here — it
// reaches into req.Config which requires a fully constructed tfsdk.Config
// only available through resource.Test machinery. Acceptance tests cover it.

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
			objectFileContentRequired().ValidateObject(context.Background(),
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
			stringNotEmpty().ValidateString(context.Background(), req, resp)
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
			v.ValidateString(context.Background(), req, resp)
			if got := resp.Diagnostics.HasError(); got != c.wantErr {
				t.Fatalf("hasError=%v want %v", got, c.wantErr)
			}
		})
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
			mapNonEmpty().ValidateMap(context.Background(), req, resp)
			if got := resp.Diagnostics.HasError(); got != c.wantErr {
				t.Fatalf("hasError=%v want %v", got, c.wantErr)
			}
		})
	}
}

func TestMapKeysMatchRegex(t *testing.T) {
	// Use the actual production regex so this test pins the path-validation
	// contract: no leading slash, no `..`, no NUL bytes, no whitespace runs.
	v := mapKeysMatchRegex(
		`^(?:[^/\s\x00.][^/\x00]*|\.[^./\x00][^/\x00]*)(?:/(?:[^/\s\x00.][^/\x00]*|\.[^./\x00][^/\x00]*))*$`,
		"file paths must be relative",
	)
	cases := []struct {
		name    string
		keys    []string
		wantErr bool
	}{
		{"plain", []string{"foo.yaml", "bar/baz.yaml"}, false},
		{"hidden-file", []string{".gitignore", ".github/CODEOWNERS"}, false},
		{"deep-path", []string{"services/frontend/values/dev.yaml"}, false},
		{"dotdot-segment", []string{"foo/../bar"}, true},
		{"leading-slash", []string{"/abs/path"}, true},
		{"single-dot-segment", []string{"./foo"}, true},
		{"nul-byte", []string{"foo\x00bar"}, true},
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
			v.ValidateMap(context.Background(), req, resp)
			if got := resp.Diagnostics.HasError(); got != c.wantErr {
				t.Fatalf("hasError=%v want %v; diags=%v", got, c.wantErr, resp.Diagnostics)
			}
		})
	}
}
