// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

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

// stringBranchName is the shared shape check for branch and ref attributes: a
// permissive character allowlist only (letters, digits, dot, underscore,
// dash, slash). It does NOT reject "..", a leading or trailing slash, or
// other git-invalid shapes; GitLab validates those server-side. This only
// blocks whitespace and exotic characters.
func stringBranchName() validator.String {
	return stringMatchesRegex(
		`^[A-Za-z0-9_./-]+$`,
		"branch name can only contain letters, digits, dot, underscore, dash, and slash",
	)
}

// stringIsBase64 rejects a content_base64 value that does not decode, at plan
// time instead of at apply time inside buildAction.
func stringIsBase64() validator.String {
	return stringIsBase64Validator{}
}

type stringIsBase64Validator struct{}

func (v stringIsBase64Validator) Description(_ context.Context) string {
	return "must be valid standard base64"
}
func (v stringIsBase64Validator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v stringIsBase64Validator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := base64.StdEncoding.DecodeString(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid base64", fmt.Sprintf("value is not valid standard base64: %s", err))
	}
}

// stringValidRepoPath applies the repository-path rules of mapKeysValidRepoPath
// to a single string attribute (the file data source's file_path).
func stringValidRepoPath() validator.String {
	return stringRepoPathValidator{}
}

type stringRepoPathValidator struct{}

func (v stringRepoPathValidator) Description(_ context.Context) string {
	return "must be a relative repository path: no leading slash, no \".\" or \"..\" segments, no NUL bytes, no empty segments, no segment starting with whitespace"
}
func (v stringRepoPathValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v stringRepoPathValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := validateRepoPath(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid file path",
			fmt.Sprintf("path %q is invalid: %s", req.ConfigValue.ValueString(), err))
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

// mapKeysValidRepoPath validates every key in the files map as a repository
// path. Explicit logic instead of a character-class regex: only the exact "."
// and ".." traversal segments are rejected, so legal git names like
// "..config" stay usable; the rest (exotic characters, length limits) is
// GitLab's to validate server-side.
func mapKeysValidRepoPath() validator.Map {
	return mapKeysRepoPathValidator{}
}

type mapKeysRepoPathValidator struct{}

func (v mapKeysRepoPathValidator) Description(_ context.Context) string {
	return "each key must be a relative repository path: no leading slash, no \".\" or \"..\" segments, no NUL bytes, no empty segments, no segment starting with whitespace"
}
func (v mapKeysRepoPathValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v mapKeysRepoPathValidator) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for k := range req.ConfigValue.Elements() {
		if err := validateRepoPath(k); err != nil {
			resp.Diagnostics.AddAttributeError(req.Path.AtMapKey(k),
				"Invalid file path",
				fmt.Sprintf("path %q is invalid: %s", k, err))
		}
	}
}

func validateRepoPath(p string) error {
	if p == "" {
		return errors.New("must not be empty")
	}
	if strings.ContainsRune(p, 0) {
		return errors.New("must not contain NUL bytes")
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" {
			return errors.New("must not contain empty segments (no leading, trailing, or doubled slashes)")
		}
		if seg == "." || seg == ".." {
			return errors.New("must not contain \".\" or \"..\" segments")
		}
		if r, _ := utf8.DecodeRuneInString(seg); unicode.IsSpace(r) {
			return errors.New("segments must not start with whitespace")
		}
	}
	return nil
}

// objectFileContentRequired enforces the "at least one" half of the
// content/content_base64 contract at plan time: a file object that sets neither
// is rejected during validate/plan instead of failing at apply with a runtime
// "either content or content_base64 must be set". Combined with the per-attribute
// stringConflictsWithSibling validators (the "not both" half), this gives the two
// fields exactly-one-of semantics before any commit is attempted.
func objectFileContentRequired() validator.Object {
	return objectFileContentRequiredValidator{}
}

type objectFileContentRequiredValidator struct{}

func (v objectFileContentRequiredValidator) Description(_ context.Context) string {
	return "either content or content_base64 must be set"
}
func (v objectFileContentRequiredValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v objectFileContentRequiredValidator) ValidateObject(_ context.Context, req validator.ObjectRequest, resp *validator.ObjectResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	attrs := req.ConfigValue.Attributes()
	content, _ := attrs["content"].(types.String)
	contentB64, _ := attrs["content_base64"].(types.String)

	// An unknown value (e.g. content fed by another resource) cannot be decided
	// yet; defer to the post-known re-validation rather than false-positive.
	if content.IsUnknown() || contentB64.IsUnknown() {
		return
	}
	if content.IsNull() && contentB64.IsNull() {
		resp.Diagnostics.AddAttributeError(req.Path,
			"Missing file content",
			"Each file must set either content or content_base64.")
	}
}
