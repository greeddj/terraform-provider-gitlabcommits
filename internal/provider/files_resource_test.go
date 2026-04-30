// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"crypto/sha1" //nolint:gosec
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// TestGitBlobSHA pins our blob SHA implementation to git's actual format
// (`blob <size>\0<content>`) so it stays compatible with GitLab's BlobID.
func TestGitBlobSHA(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"hello", "hello\n"},
		{"yaml", "app:\n  name: myapp\n"},
		{"binary-ish", "\x00\x01\x02\xff"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := sha1.New() //nolint:gosec
			fmt.Fprintf(h, "blob %d\x00", len(tt.content))
			h.Write([]byte(tt.content))
			want := hex.EncodeToString(h.Sum(nil))

			got := gitBlobSHA([]byte(tt.content))
			if got != want {
				t.Fatalf("blob sha mismatch: got %s want %s", got, want)
			}
		})
	}
}

// TestRawBytes verifies that fileModel.rawBytes returns identical bytes whether
// content was provided as text or as base64.
func TestRawBytes(t *testing.T) {
	plain := "version: 1\nname: foo\n"
	enc := base64.StdEncoding.EncodeToString([]byte(plain))

	cases := []struct {
		name string
		want string
		f    fileModel
	}{
		{name: "text", f: fileModel{Content: types.StringValue(plain)}, want: plain},
		{name: "base64", f: fileModel{ContentBase64: types.StringValue(enc)}, want: plain},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.f.rawBytes()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}

	t.Run("invalid-base64", func(t *testing.T) {
		f := fileModel{ContentBase64: types.StringValue("not-base64!!!")}
		if _, err := f.rawBytes(); err == nil {
			t.Fatal("expected error for invalid base64")
		}
	})

	t.Run("missing", func(t *testing.T) {
		f := fileModel{}
		if _, err := f.rawBytes(); err == nil {
			t.Fatal("expected error when neither content nor content_base64 is set")
		}
	})
}

// TestDiffActions exercises the central reconciliation logic: the set of
// actions emitted for any given (state, plan) pair.
func TestDiffActions(t *testing.T) {
	// helper to mint a fileModel with a deterministic blob.
	textFile := func(content string) fileModel {
		return fileModel{
			Content:         types.StringValue(content),
			ExecuteFilemode: types.BoolValue(false),
			BlobID:          types.StringValue(gitBlobSHA([]byte(content))),
		}
	}
	planFile := func(content string) fileModel {
		// Plan files don't have BlobID stamped yet (it's Computed).
		return fileModel{
			Content:         types.StringValue(content),
			ExecuteFilemode: types.BoolValue(false),
			BlobID:          types.StringNull(),
		}
	}

	cases := []struct {
		name    string
		state   map[string]fileModel
		plan    map[string]fileModel
		want    []string // "<op>:<path>"
		wantLen int
	}{
		{
			name:    "no-op",
			state:   map[string]fileModel{"a.yaml": textFile("a")},
			plan:    map[string]fileModel{"a.yaml": planFile("a")},
			want:    nil,
			wantLen: 0,
		},
		{
			name:    "create-new",
			state:   map[string]fileModel{},
			plan:    map[string]fileModel{"a.yaml": planFile("a")},
			want:    []string{"create:a.yaml"},
			wantLen: 1,
		},
		{
			name:    "delete-removed",
			state:   map[string]fileModel{"a.yaml": textFile("a")},
			plan:    map[string]fileModel{},
			want:    []string{"delete:a.yaml"},
			wantLen: 1,
		},
		{
			name:    "update-changed",
			state:   map[string]fileModel{"a.yaml": textFile("a")},
			plan:    map[string]fileModel{"a.yaml": planFile("a-modified")},
			want:    []string{"update:a.yaml"},
			wantLen: 1,
		},
		{
			name: "mixed",
			state: map[string]fileModel{
				"keep.yaml":   textFile("k"),
				"change.yaml": textFile("old"),
				"remove.yaml": textFile("r"),
			},
			plan: map[string]fileModel{
				"keep.yaml":   planFile("k"),
				"change.yaml": planFile("new"),
				"add.yaml":    planFile("a"),
			},
			want:    []string{"create:add.yaml", "update:change.yaml", "delete:remove.yaml"},
			wantLen: 3,
		},
		{
			name: "chmod-only",
			state: map[string]fileModel{
				"run.sh": {
					Content:         types.StringValue("#!/bin/sh\n"),
					ExecuteFilemode: types.BoolValue(false),
					BlobID:          types.StringValue(gitBlobSHA([]byte("#!/bin/sh\n"))),
				},
			},
			plan: map[string]fileModel{
				"run.sh": {
					Content:         types.StringValue("#!/bin/sh\n"),
					ExecuteFilemode: types.BoolValue(true),
					BlobID:          types.StringNull(),
				},
			},
			want:    []string{"chmod:run.sh"},
			wantLen: 1,
		},
	}

	r := &filesResource{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := filesResourceModel{
				ProjectID:     types.StringValue("group/proj"),
				Branch:        types.StringValue("main"),
				Files:         c.plan,
				AdoptExisting: types.BoolValue(false), // disable network calls in unit test
			}
			state := filesResourceModel{
				ProjectID: types.StringValue("group/proj"),
				Branch:    types.StringValue("main"),
				Files:     c.state,
			}
			actions, err := r.diffActions(context.Background(), plan, state)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(actions) != c.wantLen {
				t.Fatalf("got %d actions, want %d: %+v", len(actions), c.wantLen, summarise(actions))
			}
			got := summarise(actions)
			if !sliceEqual(got, c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

// TestBuildAction smoke-tests the action factory for both encodings and the
// executable bit.
func TestBuildAction(t *testing.T) {
	t.Run("text-create", func(t *testing.T) {
		a, err := buildAction("a.txt", fileModel{
			Content: types.StringValue("hi"),
		}, gitlab.FileCreate, "")
		if err != nil {
			t.Fatal(err)
		}
		if *a.Action != gitlab.FileCreate || *a.FilePath != "a.txt" || *a.Content != "hi" || *a.Encoding != "text" {
			t.Fatalf("unexpected action: %+v", a)
		}
		if a.ExecuteFilemode != nil {
			t.Fatalf("ExecuteFilemode should be unset")
		}
		if a.LastCommitID != nil {
			t.Fatalf("LastCommitID should be unset on create")
		}
	})

	t.Run("base64-update-with-exec-and-lock", func(t *testing.T) {
		enc := base64.StdEncoding.EncodeToString([]byte("hi"))
		a, err := buildAction("a.bin", fileModel{
			ContentBase64:   types.StringValue(enc),
			ExecuteFilemode: types.BoolValue(true),
		}, gitlab.FileUpdate, "abc123def")
		if err != nil {
			t.Fatal(err)
		}
		if *a.Action != gitlab.FileUpdate || *a.Encoding != "base64" || *a.Content != enc {
			t.Fatalf("unexpected action: %+v", a)
		}
		if a.ExecuteFilemode == nil || !*a.ExecuteFilemode {
			t.Fatalf("expected exec bit set")
		}
		if a.LastCommitID == nil || *a.LastCommitID != "abc123def" {
			t.Fatalf("expected last_commit_id propagated")
		}
	})

	t.Run("invalid-base64", func(t *testing.T) {
		_, err := buildAction("a.bin", fileModel{
			ContentBase64: types.StringValue("not-base64!!!"),
		}, gitlab.FileCreate, "")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("missing-content", func(t *testing.T) {
		_, err := buildAction("a.bin", fileModel{}, gitlab.FileCreate, "")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

// TestDecodeRemoteContent covers GitLab's encoding variations on the wire.
func TestDecodeRemoteContent(t *testing.T) {
	raw := []byte("hello")
	enc := base64.StdEncoding.EncodeToString(raw)

	cases := []struct {
		name string
		f    *gitlab.File
		want []byte
	}{
		{"explicit-base64", &gitlab.File{Encoding: "base64", Content: enc}, raw},
		{"empty-encoding-treated-as-base64", &gitlab.File{Encoding: "", Content: enc}, raw},
		{"explicit-text", &gitlab.File{Encoding: "text", Content: "hello"}, []byte("hello")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decodeRemoteContent(c.f)
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if string(got) != string(c.want) {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}

	t.Run("invalid-base64", func(t *testing.T) {
		_, err := decodeRemoteContent(&gitlab.File{Encoding: "base64", Content: "not-valid-base64!"})
		if err == nil {
			t.Fatal("expected error for invalid base64")
		}
	})

	t.Run("unknown-encoding", func(t *testing.T) {
		_, err := decodeRemoteContent(&gitlab.File{Encoding: "rot13", Content: "anything"})
		if err == nil {
			t.Fatal("expected error for unknown encoding")
		}
	})
}

// TestStampBlobs ensures stamping reproduces git's blob_id format for both
// text and base64 inputs, and propagates last_commit_id for optimistic locking.
func TestStampBlobs(t *testing.T) {
	files := map[string]fileModel{
		"a": {Content: types.StringValue("hello")},
		"b": {ContentBase64: types.StringValue(base64.StdEncoding.EncodeToString([]byte("hello")))},
	}
	if err := stampBlobs(files, "deadbeef"); err != nil {
		t.Fatal(err)
	}
	want := gitBlobSHA([]byte("hello"))
	if files["a"].BlobID.ValueString() != want {
		t.Errorf("a: %s != %s", files["a"].BlobID.ValueString(), want)
	}
	if files["b"].BlobID.ValueString() != want {
		t.Errorf("b: %s != %s", files["b"].BlobID.ValueString(), want)
	}
	if files["a"].LastCommitID.ValueString() != "deadbeef" {
		t.Errorf("a: last_commit_id not stamped, got %q", files["a"].LastCommitID.ValueString())
	}
	if files["b"].LastCommitID.ValueString() != "deadbeef" {
		t.Errorf("b: last_commit_id not stamped, got %q", files["b"].LastCommitID.ValueString())
	}
}

// TestDiffActions_OptimisticLock verifies that update / delete / chmod actions
// carry the file's last_commit_id from state when optimistic_lock is enabled,
// and don't carry it when disabled.
func TestDiffActions_OptimisticLock(t *testing.T) {
	r := &filesResource{}

	state := filesResourceModel{
		ProjectID: types.StringValue("group/proj"),
		Branch:    types.StringValue("main"),
		Files: map[string]fileModel{
			"a.yaml": {
				Content:         types.StringValue("v1"),
				ExecuteFilemode: types.BoolValue(false),
				BlobID:          types.StringValue(gitBlobSHA([]byte("v1"))),
				LastCommitID:    types.StringValue("commit-A"),
			},
			"b.yaml": {
				Content:         types.StringValue("keep"),
				ExecuteFilemode: types.BoolValue(false),
				BlobID:          types.StringValue(gitBlobSHA([]byte("keep"))),
				LastCommitID:    types.StringValue("commit-B"),
			},
		},
	}

	plan := filesResourceModel{
		ProjectID:     types.StringValue("group/proj"),
		Branch:        types.StringValue("main"),
		AdoptExisting: types.BoolValue(false),
		Files: map[string]fileModel{
			"a.yaml": { // updated
				Content:         types.StringValue("v2"),
				ExecuteFilemode: types.BoolValue(false),
			},
			// b.yaml gone → delete
		},
	}

	t.Run("enabled-default", func(t *testing.T) {
		// OptimisticLock null → defaults to true.
		actions, err := r.diffActions(context.Background(), plan, state)
		if err != nil {
			t.Fatal(err)
		}
		got := lastCommitIDs(actions)
		want := map[string]string{"a.yaml": "commit-A", "b.yaml": "commit-B"}
		for path, wantSHA := range want {
			if got[path] != wantSHA {
				t.Errorf("path=%q got last_commit_id=%q want %q", path, got[path], wantSHA)
			}
		}
	})

	t.Run("disabled", func(t *testing.T) {
		planNoLock := plan
		planNoLock.OptimisticLock = types.BoolValue(false)
		actions, err := r.diffActions(context.Background(), planNoLock, state)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range actions {
			if a.LastCommitID != nil {
				t.Errorf("expected no last_commit_id when optimistic_lock=false, got %q for %s", *a.LastCommitID, *a.FilePath)
			}
		}
	})
}

// lastCommitIDs returns a path → last_commit_id map for actions, empty string
// when an action has no LastCommitID set.
func lastCommitIDs(actions []*gitlab.CommitActionOptions) map[string]string {
	out := make(map[string]string, len(actions))
	for _, a := range actions {
		if a.LastCommitID == nil {
			out[*a.FilePath] = ""
			continue
		}
		out[*a.FilePath] = *a.LastCommitID
	}
	return out
}

// TestSortedKeys guarantees deterministic ordering of actions across runs,
// which makes commits reproducible and diffs reviewable.
func TestSortedKeys(t *testing.T) {
	m := map[string]int{"c": 1, "a": 2, "b": 3}
	got := sortedKeys(m)
	want := []string{"a", "b", "c"}
	if !sliceEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestBuildID pins the composite import/state ID format.
func TestBuildID(t *testing.T) {
	id := buildID("group/proj", "main")
	if id != "group/proj::main" {
		t.Fatalf("got %s", id)
	}
}

// TestParseImportID covers the import ID parser used by ImportState. It must
// reject anything that does not split cleanly into exactly two non-empty
// halves so the caller never silently keeps part of the suffix.
func TestParseImportID(t *testing.T) {
	cases := []struct {
		in       string
		wantProj string
		wantBr   string
		wantErr  bool
	}{
		{"a::b", "a", "b", false},
		{"group/sub/proj::main", "group/sub/proj", "main", false},
		{"123::release/v1", "123", "release/v1", false},
		{"", "", "", true},
		{"::", "", "", true},
		{"a::", "", "", true},
		{"::b", "", "", true},
		{"a::b::c", "", "", true},
		{"no-separator", "", "", true},
		{"   ::main", "", "", true},
		{"proj::   ", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			p, b, err := parseImportID(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if !c.wantErr && (p != c.wantProj || b != c.wantBr) {
				t.Fatalf("got %q/%q want %q/%q", p, b, c.wantProj, c.wantBr)
			}
		})
	}
}

// TestApiErrorDiag pins the HTTP-status → diagnostic mapping. Each branch
// (401/403/404/400/409/429/default) carries actionable wording for the user;
// asserting the summary plus key detail substrings here keeps the wording from
// drifting silently and locks in the truncation + Retry-After behaviour.
func TestApiErrorDiag(t *testing.T) {
	mkErr := func(status int, msg string, headers http.Header) error {
		if headers == nil {
			headers = http.Header{}
		}
		return &gitlab.ErrorResponse{
			Response: &http.Response{StatusCode: status, Header: headers},
			Message:  msg,
		}
	}

	cases := []struct {
		err         error
		name        string
		wantSummary string
		contains    []string
	}{
		{
			name: "401", err: mkErr(401, "invalid token", nil),
			wantSummary: "GitLab authentication failed (HTTP 401)",
			contains:    []string{"token rejected", "invalid token"},
		},
		{
			name: "403", err: mkErr(403, "forbidden", nil),
			wantSummary: "GitLab permission denied (HTTP 403)",
			contains:    []string{"write_repository"},
		},
		{
			name: "404", err: mkErr(404, "not found", nil),
			wantSummary: "GitLab resource not found (HTTP 404)",
			contains:    []string{"does not exist"},
		},
		{
			name: "400-conflict-snake_case", err: mkErr(400, "last_commit_id mismatch", nil),
			wantSummary: "Concurrent modification detected (optimistic_lock)",
			contains:    []string{"refresh-only"},
		},
		{
			name: "400-conflict-prose-mixed-case", err: mkErr(400, "Wrong Last Commit detected", nil),
			wantSummary: "Concurrent modification detected (optimistic_lock)",
			contains:    []string{"refresh-only"},
		},
		{
			name: "400-other", err: mkErr(400, "validation failed", nil),
			wantSummary: "GitLab API error: act",
			contains:    []string{"HTTP 400", "validation failed"},
		},
		{
			name: "409-conflict", err: mkErr(409, "last commit changed", nil),
			wantSummary: "Concurrent modification detected (optimistic_lock)",
			contains:    []string{"refresh-only"},
		},
		{
			name: "429-with-retry-after", err: mkErr(429, "rate limited", http.Header{"Retry-After": []string{"60"}}),
			wantSummary: "GitLab rate limit exceeded (HTTP 429)",
			contains:    []string{"retry after 60 seconds"},
		},
		{
			name: "429-without-retry-after", err: mkErr(429, "rate limited", nil),
			wantSummary: "GitLab rate limit exceeded (HTTP 429)",
			contains:    []string{"retry after unknown seconds"},
		},
		{
			name: "500-default", err: mkErr(500, "boom", nil),
			wantSummary: "GitLab API error: act",
			contains:    []string{"HTTP 500", "boom"},
		},
		{
			name: "plain-error", err: errors.New("connection refused"),
			wantSummary: "GitLab API error: act",
			contains:    []string{"connection refused"},
		},
		{
			name: "truncated-body", err: mkErr(400, strings.Repeat("x", 2000), nil),
			wantSummary: "GitLab API error: act",
			contains:    []string{"truncated, 976 more chars"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			summary, detail := apiErrorDiag("act", "proj", "main", c.err)
			if summary != c.wantSummary {
				t.Errorf("summary = %q, want %q", summary, c.wantSummary)
			}
			for _, sub := range c.contains {
				if !strings.Contains(detail, sub) {
					t.Errorf("detail missing %q; full detail: %s", sub, detail)
				}
			}
			if !strings.Contains(detail, `project="proj"`) || !strings.Contains(detail, `branch="main"`) {
				t.Errorf("detail missing project/branch prefix: %s", detail)
			}
		})
	}

}

// helpers

func summarise(actions []*gitlab.CommitActionOptions) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, fmt.Sprintf("%s:%s", *a.Action, *a.FilePath))
	}
	return out
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
