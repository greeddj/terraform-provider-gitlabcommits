// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

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
	// textFile is a state-side file — content set, blob opaque (as stored after stampBlobs).
	textFile := func(content string) fileModel {
		return fileModel{
			Content:         types.StringValue(content),
			ExecuteFilemode: types.BoolValue(false),
			BlobID:          types.StringValue("opaque-blob-id-" + content),
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
					BlobID:          types.StringValue("opaque-blob-id-run.sh"),
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

// TestDiffActions_ContentEqualBytewise verifies that identical content expressed
// via different attributes (content vs content_base64) produces no action.
func TestDiffActions_ContentEqualBytewise(t *testing.T) {
	r := &filesResource{}

	state := filesResourceModel{
		ProjectID: types.StringValue("group/proj"),
		Branch:    types.StringValue("main"),
		Files: map[string]fileModel{
			"a.txt": {
				Content:         types.StringValue("hello"),
				ExecuteFilemode: types.BoolValue(false),
				BlobID:          types.StringValue("opaque-anything"),
			},
		},
	}
	plan := filesResourceModel{
		ProjectID:     types.StringValue("group/proj"),
		Branch:        types.StringValue("main"),
		AdoptExisting: types.BoolValue(false),
		Files: map[string]fileModel{
			"a.txt": {
				ContentBase64:   types.StringValue(base64.StdEncoding.EncodeToString([]byte("hello"))),
				ExecuteFilemode: types.BoolValue(false),
				BlobID:          types.StringNull(),
			},
		},
	}

	actions, err := r.diffActions(context.Background(), plan, state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected no actions for bytewise-equal content, got %d: %v", len(actions), summarise(actions))
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

// TestStampBlobsAfterCommit_FetchesMetadata verifies that stampBlobs issues
// parallel HEAD probes and populates BlobID from the X-Gitlab-Blob-Id header.
// LastCommitID must come from the probe's X-Gitlab-Last-Commit-Id, not the
// commitSHA argument, so that a concurrent writer between CreateCommit and the
// HEAD probe is reflected in state.
//
// The test is parameterised on blob-id format to prove that stampBlobs is
// opaque to blob-id length: both 40-char SHA-1 and 64-char SHA-256 IDs must
// round-trip cleanly through state.
func TestStampBlobsAfterCommit_FetchesMetadata(t *testing.T) {
	cases := []struct {
		name   string
		blobID string
	}{
		{
			name:   "sha1",
			blobID: "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391", // git empty-blob SHA-1
		},
		{
			name:   "sha256",
			blobID: "9d3b8e6bfdcaee5a3b9b8e6c1d4e3c8a7f8b2c5d6e7f8a9b0c1d2e3f4a5b6c7d",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Per-path blob IDs and server-side last-commit-ids the stub will return.
			// These server-commit values are distinct from "deadbeef" (our commitSHA arg)
			// so a regression that silently falls back to commitSHA fails the assertion.
			type pathMeta struct {
				blobID       string
				lastCommitID string
			}
			metaByPath := map[string]pathMeta{
				"a.txt": {blobID: tc.blobID, lastCommitID: "server-commit-001"},
				"b.txt": {blobID: tc.blobID, lastCommitID: "server-commit-002"},
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					http.Error(w, "expected HEAD", http.StatusMethodNotAllowed)
					return
				}
				// URL pattern: /api/v4/projects/proj/repository/files/<encoded-path>
				for p, m := range metaByPath {
					if strings.Contains(r.URL.Path, p) {
						w.Header().Set("X-Gitlab-Blob-Id", m.blobID)
						w.Header().Set("X-Gitlab-Last-Commit-Id", m.lastCommitID)
						w.Header().Set("X-Gitlab-Commit-Id", m.lastCommitID)
						w.Header().Set("X-Gitlab-File-Path", p)
						w.Header().Set("X-Gitlab-Ref", "main")
						w.Header().Set("X-Gitlab-Size", "5")
						w.WriteHeader(http.StatusOK)
						return
					}
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()

			client, err := gitlab.NewClient("tok", gitlab.WithBaseURL(srv.URL+"/"))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}

			res := &filesResource{client: client}

			files := map[string]fileModel{
				"a.txt": {Content: types.StringValue("hello"), BlobID: types.StringNull()},
				"b.txt": {Content: types.StringValue("world"), BlobID: types.StringNull()},
			}

			diags := res.stampBlobs(context.Background(), "proj", "main", files, "deadbeef")
			for _, d := range diags {
				if d.Severity() == 1 { // error
					t.Errorf("unexpected error diagnostic: %s: %s", d.Summary(), d.Detail())
				}
			}

			for p, wantMeta := range metaByPath {
				f, ok := files[p]
				if !ok {
					t.Fatalf("path %q missing from files map after stampBlobs", p)
				}
				if f.BlobID.ValueString() != wantMeta.blobID {
					t.Errorf("path %q: BlobID = %q, want %q", p, f.BlobID.ValueString(), wantMeta.blobID)
				}
				// LastCommitID must come from the server probe, not from "deadbeef".
				if f.LastCommitID.ValueString() != wantMeta.lastCommitID {
					t.Errorf("path %q: LastCommitID = %q, want %q (server value)", p, f.LastCommitID.ValueString(), wantMeta.lastCommitID)
				}
			}
		})
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
				BlobID:          types.StringValue("opaque-blob-a"),
				LastCommitID:    types.StringValue("commit-A"),
			},
			"b.yaml": {
				Content:         types.StringValue("keep"),
				ExecuteFilemode: types.BoolValue(false),
				BlobID:          types.StringValue("opaque-blob-b"),
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
			contains:    []string{"api", "Developer", "CI_JOB_TOKEN"},
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
			// Verbatim GitLab 18 message for optimistic-lock failure.
			name: "400-gitlab18-verbatim",
			err: mkErr(400,
				"You are attempting to update a file that has changed since you started editing it. Try again. File last commit id: 8a2b3c4d", nil),
			wantSummary: "Concurrent modification detected (optimistic_lock)",
			contains:    []string{"refresh-only"},
		},
		{
			// Fictional message that contains only "has changed since" — no overlap
			// with "last_commit_id" or "last commit". Falsifies the third substring
			// branch in apiErrorDiag independently of the other two matchers.
			name: "400-has-changed-since-only",
			err: mkErr(400,
				"The remote file has changed since you started editing. Please reload.", nil),
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
			name: "413-too-large",
			err: mkErr(413,
				"RequestBody: upload failed: the upload size is over maximum of 314572800 bytes", nil),
			wantSummary: "GitLab commit too large (HTTP 413)",
			contains:    []string{"300 MB", "for_each"},
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

// TestStampBlobs_OneProbeFailure verifies the fail-soft contract: when one
// HEAD probe returns a server error, stampBlobs appends a warning (not an
// error), leaves that file's BlobID null, and still stamps BlobID correctly
// for the file whose probe succeeded.
//
// LastCommitID diverges between the two paths:
//   - good-path: comes from the probe's X-Gitlab-Last-Commit-Id ("server-commit-good")
//   - bad-path:  falls back to commitSHA ("deadbeef") because the probe failed
//
// This uniquely covers the fail-soft path of the new behaviour.
func TestStampBlobs_OneProbeFailure(t *testing.T) {
	const (
		goodBlob           = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
		goodServerCommitID = "server-commit-good"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			http.Error(w, "expected HEAD", http.StatusMethodNotAllowed)
			return
		}
		// Route by path segment: "bad-path" → 500, "good-path" → 200 with blob header.
		if strings.Contains(r.URL.Path, "bad-path") {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if strings.Contains(r.URL.Path, "good-path") {
			w.Header().Set("X-Gitlab-Blob-Id", goodBlob)
			w.Header().Set("X-Gitlab-Last-Commit-Id", goodServerCommitID)
			w.Header().Set("X-Gitlab-Commit-Id", goodServerCommitID)
			w.Header().Set("X-Gitlab-File-Path", "good-path")
			w.Header().Set("X-Gitlab-Ref", "main")
			w.Header().Set("X-Gitlab-Size", "5")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client, err := gitlab.NewClient("tok", gitlab.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res := &filesResource{client: client}

	files := map[string]fileModel{
		"bad-path":  {Content: types.StringValue("bad"), BlobID: types.StringNull()},
		"good-path": {Content: types.StringValue("good"), BlobID: types.StringNull()},
	}

	diags := res.stampBlobs(context.Background(), "proj", "main", files, "deadbeef")

	if diags.HasError() {
		t.Fatalf("expected no errors, got: %v", diags.Errors())
	}
	if n := diags.WarningsCount(); n != 1 {
		t.Fatalf("expected 1 warning, got %d", n)
	}
	w := diags.Warnings()[0]
	if !strings.Contains(w.Summary(), "Could not refresh blob_id after commit") {
		t.Errorf("warning summary = %q, want it to contain %q", w.Summary(), "Could not refresh blob_id after commit")
	}
	if !strings.Contains(w.Detail(), "bad-path") {
		t.Errorf("warning detail = %q, want it to mention %q", w.Detail(), "bad-path")
	}

	bad := files["bad-path"]
	if !bad.BlobID.IsNull() {
		t.Errorf("bad-path BlobID should be null after probe failure, got %q", bad.BlobID.ValueString())
	}
	// Probe failed → falls back to commitSHA.
	if bad.LastCommitID.ValueString() != "deadbeef" {
		t.Errorf("bad-path LastCommitID = %q, want %q (commitSHA fallback)", bad.LastCommitID.ValueString(), "deadbeef")
	}

	good := files["good-path"]
	if good.BlobID.ValueString() != goodBlob {
		t.Errorf("good-path BlobID = %q, want %q", good.BlobID.ValueString(), goodBlob)
	}
	// Probe succeeded → LastCommitID comes from the server, not commitSHA.
	if good.LastCommitID.ValueString() != goodServerCommitID {
		t.Errorf("good-path LastCommitID = %q, want %q (server value)", good.LastCommitID.ValueString(), goodServerCommitID)
	}
}

// TestStampBlobs_OversizedBlobIDRejected verifies the defensive length cap: when
// the server returns a blob_id longer than 256 bytes (anything above SHA-512 hex
// is unexpected and could indicate a hostile/MITM'd response), stampBlobs treats
// the probe as failed: BlobID is null, LastCommitID falls back to commitSHA, and
// a warning (not an error) is emitted.
func TestStampBlobs_OversizedBlobIDRejected(t *testing.T) {
	oversized := strings.Repeat("a", 300)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			http.Error(w, "expected HEAD", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Gitlab-Blob-Id", oversized)
		w.Header().Set("X-Gitlab-Last-Commit-Id", "should-not-appear")
		w.Header().Set("X-Gitlab-Commit-Id", "should-not-appear")
		w.Header().Set("X-Gitlab-File-Path", "file.txt")
		w.Header().Set("X-Gitlab-Ref", "main")
		w.Header().Set("X-Gitlab-Size", "5")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := gitlab.NewClient("tok", gitlab.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res := &filesResource{client: client}

	files := map[string]fileModel{
		"file.txt": {Content: types.StringValue("hello"), BlobID: types.StringNull()},
	}

	diags := res.stampBlobs(context.Background(), "proj", "main", files, "deadbeef")

	if diags.HasError() {
		t.Fatalf("expected no errors, got: %v", diags.Errors())
	}
	if n := diags.WarningsCount(); n != 1 {
		t.Fatalf("expected 1 warning, got %d", n)
	}
	w := diags.Warnings()[0]
	if !strings.Contains(w.Detail(), "file.txt") {
		t.Errorf("warning detail %q should mention the path %q", w.Detail(), "file.txt")
	}
	if !strings.Contains(w.Detail(), "300") {
		t.Errorf("warning detail %q should mention the length %d", w.Detail(), 300)
	}

	f := files["file.txt"]
	if !f.BlobID.IsNull() {
		t.Errorf("BlobID should be null for oversized blob_id, got %q", f.BlobID.ValueString())
	}
	// Probe treated as failure → commitSHA fallback for LastCommitID.
	if f.LastCommitID.ValueString() != "deadbeef" {
		t.Errorf("LastCommitID = %q, want %q (commitSHA fallback)", f.LastCommitID.ValueString(), "deadbeef")
	}
}

// TestStampBlobs_RaceDetectedViaServerLastCommitID is the directly-falsifying
// test for the security fix: if another writer commits between our CreateCommit
// and the HEAD probe, the server returns their LastCommitID. stampBlobs must
// preserve that server value in state (not overwrite it with our commitSHA) so
// optimistic_lock catches the race on the next apply.
func TestStampBlobs_RaceDetectedViaServerLastCommitID(t *testing.T) {
	const (
		ourCommitSHA      = "ours"
		racerLastCommitID = "raced-by-someone-else"
		racerBlobID       = "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			http.Error(w, "expected HEAD", http.StatusMethodNotAllowed)
			return
		}
		// Simulate the racer's commit being visible at HEAD: different blob
		// and last_commit_id from what we just committed.
		w.Header().Set("X-Gitlab-Blob-Id", racerBlobID)
		w.Header().Set("X-Gitlab-Last-Commit-Id", racerLastCommitID)
		w.Header().Set("X-Gitlab-Commit-Id", racerLastCommitID)
		w.Header().Set("X-Gitlab-File-Path", "file.txt")
		w.Header().Set("X-Gitlab-Ref", "main")
		w.Header().Set("X-Gitlab-Size", "5")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := gitlab.NewClient("tok", gitlab.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res := &filesResource{client: client}

	files := map[string]fileModel{
		"file.txt": {Content: types.StringValue("hello"), BlobID: types.StringNull()},
	}

	diags := res.stampBlobs(context.Background(), "proj", "main", files, ourCommitSHA)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags.Errors())
	}

	f := files["file.txt"]
	// The racer's LastCommitID must be preserved, not our commitSHA.
	if f.LastCommitID.ValueString() != racerLastCommitID {
		t.Errorf("LastCommitID = %q, want %q (racer's server value)", f.LastCommitID.ValueString(), racerLastCommitID)
	}
	if f.BlobID.ValueString() != racerBlobID {
		t.Errorf("BlobID = %q, want %q (racer's blob)", f.BlobID.ValueString(), racerBlobID)
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
