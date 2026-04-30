// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// stringConflictsWithSibling enforces that an attribute cannot be set when a
// sibling attribute (within the same nested object) is also set. Used for the
// content / content_base64 mutual-exclusion.
func stringConflictsWithSibling(siblingAttributeName string) validator.String {
	return stringConflictsWithSiblingValidator{siblingAttributeName: siblingAttributeName}
}

type stringConflictsWithSiblingValidator struct {
	siblingAttributeName string
}

func (v stringConflictsWithSiblingValidator) Description(_ context.Context) string {
	return fmt.Sprintf("cannot be set when %s is set", v.siblingAttributeName)
}

func (v stringConflictsWithSiblingValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v stringConflictsWithSiblingValidator) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}

	var sibling types.String
	diags := req.Config.GetAttribute(ctx, req.Path.ParentPath().AtName(v.siblingAttributeName), &sibling)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	if sibling.IsNull() || sibling.IsUnknown() {
		return
	}

	resp.Diagnostics.AddAttributeError(
		req.Path,
		"Conflicting configuration",
		fmt.Sprintf("Attribute %q cannot be set when %q is set.", req.Path.String(), v.siblingAttributeName),
	)
}

// stringNotEmpty rejects empty / whitespace-only strings.
func stringNotEmpty() validator.String {
	return stringNotEmptyValidator{}
}

type stringNotEmptyValidator struct{}

func (v stringNotEmptyValidator) Description(_ context.Context) string {
	return "must be non-empty"
}
func (v stringNotEmptyValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v stringNotEmptyValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if strings.TrimSpace(req.ConfigValue.ValueString()) == "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Empty value", "value must be non-empty")
	}
}

// stringMatchesRegex rejects values that do not match the provided pattern.
// The message is shown to the user verbatim, so write it from the user's
// perspective ("must look like ...").
func stringMatchesRegex(pattern, msg string) validator.String {
	return stringRegexValidator{re: regexp.MustCompile(pattern), msg: msg, pattern: pattern}
}

type stringRegexValidator struct {
	re      *regexp.Regexp
	msg     string
	pattern string
}

func (v stringRegexValidator) Description(_ context.Context) string {
	return fmt.Sprintf("must match %s (%s)", v.pattern, v.msg)
}
func (v stringRegexValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v stringRegexValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if !v.re.MatchString(req.ConfigValue.ValueString()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid value", v.msg)
	}
}

// mapNonEmpty rejects empty maps. Used to keep `files` from being silently
// configured as `{}`, which would otherwise translate to "delete everything"
// on update — almost never what the user wants.
func mapNonEmpty() validator.Map {
	return mapNonEmptyValidator{}
}

type mapNonEmptyValidator struct{}

func (v mapNonEmptyValidator) Description(_ context.Context) string {
	return "must contain at least one entry"
}
func (v mapNonEmptyValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v mapNonEmptyValidator) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if len(req.ConfigValue.Elements()) == 0 {
		resp.Diagnostics.AddAttributeError(req.Path, "Empty files map",
			"`files` must contain at least one entry. To remove all managed files, destroy the resource instead.")
	}
}

// mapKeysMatchRegex validates every key in a map against a pattern. Used for
// repository file paths: no leading slash, no `..`, no NUL bytes.
func mapKeysMatchRegex(pattern, msg string) validator.Map {
	return mapKeysRegexValidator{re: regexp.MustCompile(pattern), msg: msg, pattern: pattern}
}

type mapKeysRegexValidator struct {
	re      *regexp.Regexp
	msg     string
	pattern string
}

func (v mapKeysRegexValidator) Description(_ context.Context) string {
	return fmt.Sprintf("each key must match %s (%s)", v.pattern, v.msg)
}
func (v mapKeysRegexValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v mapKeysRegexValidator) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for k := range req.ConfigValue.Elements() {
		if !v.re.MatchString(k) {
			resp.Diagnostics.AddAttributeError(req.Path.AtMapKey(k),
				"Invalid file path",
				fmt.Sprintf("path %q is invalid: %s", k, v.msg))
		}
	}
}
