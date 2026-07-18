// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// readState is a minimal one-file state used to drive filesResource.Read.
func readState(blobID string) filesResourceModel {
	return filesResourceModel{
		ID:               types.StringValue("proj::main"),
		ProjectID:        types.StringValue("proj"),
		Branch:           types.StringValue("main"),
		CommitMessage:    types.StringValue("msg"),
		CommitSHA:        types.StringValue("sha"),
		AuthorEmail:      types.StringNull(),
		AuthorName:       types.StringNull(),
		CreateBranchFrom: types.StringNull(),
		DetectDrift:      types.BoolValue(true),
		DeleteOnDestroy:  types.BoolValue(true),
		AdoptExisting:    types.BoolValue(true),
		OptimisticLock:   types.BoolValue(true),
		Files: map[string]fileModel{
			"f.txt": {
				Content:         types.StringValue("old"),
				ContentBase64:   types.StringNull(),
				BlobID:          types.StringValue(blobID),
				LastCommitID:    types.StringValue("oldlcid"),
				ExecuteFilemode: types.BoolValue(false),
			},
		},
	}
}

// runRead builds a ReadRequest from the model and invokes Read, returning the
// response and (on success) the resulting state model.
func runRead(t *testing.T, client *gitlab.Client, state filesResourceModel) (*resource.ReadResponse, filesResourceModel) {
	t.Helper()
	ctx := context.Background()
	res := &filesResource{client: client}

	sresp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, sresp)
	sch := sresp.Schema

	st := tfsdk.State{Schema: sch}
	if d := st.Set(ctx, &state); d.HasError() {
		t.Fatalf("state.Set: %v", d)
	}

	req := resource.ReadRequest{State: st}
	resp := &resource.ReadResponse{State: tfsdk.State{Schema: sch}}
	res.Read(ctx, req, resp)

	var out filesResourceModel
	if !resp.Diagnostics.HasError() {
		resp.State.Get(ctx, &out)
	}
	return resp, out
}

func newReadClient(t *testing.T, h http.HandlerFunc) *gitlab.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	// Retries off: unit tests assert on single-shot behavior, and a faked 5xx
	// would otherwise stall every assertion behind the full backoff schedule.
	client, err := gitlab.NewClient("tok", gitlab.WithBaseURL(srv.URL+"/"), gitlab.WithoutRetries())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// TestRead_DropsMissingFile covers the drift drop-pass (tests-coverage A): a
// managed file that 404s on the metadata probe is removed from state so the
// next plan recreates it. The branch itself still exists, so the resource
// stays in state.
func TestRead_DropsMissingFile(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"head"}}`))
			return
		}
		http.Error(w, "unexpected GET", http.StatusInternalServerError)
	})

	resp, out := runRead(t, client, readState("oldblob"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("resource must stay in state while the branch exists")
	}
	if _, ok := out.Files["f.txt"]; ok {
		t.Fatalf("expected f.txt to be dropped from state, still present")
	}
}

// TestRead_BranchGoneRemovesResource: when every managed file vanishes AND the
// branch itself 404s, the resource must be removed from state (with a warning)
// instead of surviving as a stranded shell with an empty files map.
func TestRead_BranchGoneRemovesResource(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})

	resp, _ := runRead(t, client, readState("oldblob"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("expected the resource to be removed from state when the branch is gone")
	}
	if len(resp.Diagnostics) == 0 {
		t.Error("expected a warning diagnostic explaining the removal")
	}
}

// TestRead_BranchCheckErrorFails: if the confirming branch lookup fails with a
// non-404, Read must error instead of guessing between "deleted" and
// "temporarily unreachable".
func TestRead_BranchCheckErrorFails(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	})

	resp, _ := runRead(t, client, readState("oldblob"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error when the branch check fails")
	}
}

// TestRead_DriftUpdatesContent covers the drift repopulate-pass: a changed
// blob_id triggers GetFile and the new content lands in state.
func TestRead_DriftUpdatesContent(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("X-Gitlab-Blob-Id", "newblob")
			w.Header().Set("X-Gitlab-Last-Commit-Id", "newlcid")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "main")
			w.Header().Set("X-Gitlab-Size", "3")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"file_path":"f.txt","blob_id":"newblob","content":"` +
			base64.StdEncoding.EncodeToString([]byte("new")) + `","encoding":"base64","last_commit_id":"newlcid","size":3}`))
	})

	resp, out := runRead(t, client, readState("oldblob"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	f := out.Files["f.txt"]
	if f.Content.ValueString() != "new" {
		t.Errorf("content = %q, want %q", f.Content.ValueString(), "new")
	}
	if f.BlobID.ValueString() != "newblob" {
		t.Errorf("blob_id = %q, want %q", f.BlobID.ValueString(), "newblob")
	}
	if f.LastCommitID.ValueString() != "newlcid" {
		t.Errorf("last_commit_id = %q, want %q (restamped on drift)", f.LastCommitID.ValueString(), "newlcid")
	}
}

// TestRead_NullFileBodyOnDriftErrors pins ADD-4: a 2xx JSON-null GetFile body for
// a drifted blob must surface an error, not be silently treated as unchanged.
func TestRead_NullFileBodyOnDriftErrors(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("X-Gitlab-Blob-Id", "newblob")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "main")
			w.Header().Set("X-Gitlab-Size", "3")
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte("null"))
	})

	resp, _ := runRead(t, client, readState("oldblob"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for a null GetFile body on a drifted blob")
	}
}

// TestRead_OversizedBlobIDIgnored pins CRU-2: an absurdly long blob_id from
// GetFile is not persisted; state keeps blob_id null and a warning is emitted.
func TestRead_OversizedBlobIDIgnored(t *testing.T) {
	oversized := strings.Repeat("a", maxBlobIDLen+1)
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("X-Gitlab-Blob-Id", "newblob")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "main")
			w.Header().Set("X-Gitlab-Size", "3")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"file_path":"f.txt","blob_id":"` + oversized + `","content":"` +
			base64.StdEncoding.EncodeToString([]byte("new")) + `","encoding":"base64","last_commit_id":"newlcid","size":3}`))
	})

	resp, out := runRead(t, client, readState("oldblob"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if resp.Diagnostics.WarningsCount() == 0 {
		t.Error("expected a warning for the oversized blob_id")
	}
	if f := out.Files["f.txt"]; !f.BlobID.IsNull() {
		t.Errorf("blob_id = %q, want null (oversized ignored)", f.BlobID.ValueString())
	}
}

// TestRead_BinaryDriftIntoContentErrors: a file managed via the text content
// attribute that drifts to invalid UTF-8 must produce an error diagnostic
// pointing at content_base64 - cty would silently mangle the bytes to U+FFFD
// and the diff could never converge.
func TestRead_BinaryDriftIntoContentErrors(t *testing.T) {
	binary := []byte{0xff, 0xfe, 0x00, 0x01}
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("X-Gitlab-Blob-Id", "newblob")
			w.Header().Set("X-Gitlab-Last-Commit-Id", "newlcid")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "main")
			w.Header().Set("X-Gitlab-Size", "4")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"file_path":"f.txt","blob_id":"newblob","content":"` +
			base64.StdEncoding.EncodeToString(binary) + `","encoding":"base64","last_commit_id":"newlcid","size":4}`))
	})

	resp, _ := runRead(t, client, readState("oldblob"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic for binary drift into a text-managed file")
	}
	found := false
	for _, d := range resp.Diagnostics.Errors() {
		if strings.Contains(d.Detail(), "content_base64") {
			found = true
		}
	}
	if !found {
		t.Error("expected the diagnostic to point the user at content_base64")
	}
}

// TestRead_UnchangedBlobSkipsGetFile pins the core drift-detection invariant:
// when the HEAD probe reports the same blob_id and exec bit as state, Read
// must not download content - and must refresh last_commit_id only from the
// probe (a delete-then-re-add with identical content moves the commit id
// while keeping the blob).
func TestRead_UnchangedBlobSkipsGetFile(t *testing.T) {
	cases := []struct {
		name      string
		probeLCID string
	}{
		{"lcid moved is restamped", "moved-lcid"},
		{"lcid unchanged stays", "oldlcid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.Header().Set("X-Gitlab-Blob-Id", "oldblob")
					w.Header().Set("X-Gitlab-Last-Commit-Id", tc.probeLCID)
					w.Header().Set("X-Gitlab-File-Path", "f.txt")
					w.Header().Set("X-Gitlab-Ref", "main")
					w.Header().Set("X-Gitlab-Size", "3")
					w.WriteHeader(http.StatusOK)
					return
				}
				t.Errorf("unexpected %s %s: an unchanged blob must not fetch content", r.Method, r.URL.Path)
				http.Error(w, "no", http.StatusInternalServerError)
			})

			resp, out := runRead(t, client, readState("oldblob"))
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			f := out.Files["f.txt"]
			if f.LastCommitID.ValueString() != tc.probeLCID {
				t.Errorf("LastCommitID = %q, want %q", f.LastCommitID.ValueString(), tc.probeLCID)
			}
			if f.Content.ValueString() != "old" {
				t.Errorf("content = %q, want untouched %q", f.Content.ValueString(), "old")
			}
			if f.BlobID.ValueString() != "oldblob" {
				t.Errorf("BlobID = %q, want untouched %q", f.BlobID.ValueString(), "oldblob")
			}
		})
	}
}

// TestRead_DetectDriftFalseNoAPICalls: detect_drift=false makes Read a pure
// state pass-through with zero API traffic.
func TestRead_DetectDriftFalseNoAPICalls(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call %s %s with detect_drift=false", r.Method, r.URL.Path)
		http.Error(w, "no", http.StatusInternalServerError)
	})
	state := readState("blob")
	state.DetectDrift = types.BoolValue(false)

	resp, out := runRead(t, client, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	f := out.Files["f.txt"]
	if f.Content.ValueString() != "old" || f.BlobID.ValueString() != "blob" {
		t.Errorf("state must be preserved verbatim, got content=%q blob=%q", f.Content.ValueString(), f.BlobID.ValueString())
	}
}

// TestRead_Base64FormPreservedOnDrift: a file managed via content_base64 keeps
// that form on refresh, and binary bytes are legal there.
func TestRead_Base64FormPreservedOnDrift(t *testing.T) {
	binary := []byte{0xff, 0xfe, 0x00, 0x01}
	encoded := base64.StdEncoding.EncodeToString(binary)
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("X-Gitlab-Blob-Id", "newblob")
			w.Header().Set("X-Gitlab-Last-Commit-Id", "newlcid")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "main")
			w.Header().Set("X-Gitlab-Size", "4")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"file_path":"f.txt","blob_id":"newblob","content":"` + encoded + `","encoding":"base64","last_commit_id":"newlcid","size":4}`))
	})

	state := readState("oldblob")
	f := state.Files["f.txt"]
	f.Content = types.StringNull()
	f.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString([]byte("old")))
	state.Files["f.txt"] = f

	resp, out := runRead(t, client, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	got := out.Files["f.txt"]
	if !got.Content.IsNull() {
		t.Errorf("content must stay null for a base64-managed file, got %q", got.Content.ValueString())
	}
	if got.ContentBase64.ValueString() != encoded {
		t.Errorf("content_base64 = %q, want %q", got.ContentBase64.ValueString(), encoded)
	}
	if got.BlobID.ValueString() != "newblob" {
		t.Errorf("BlobID = %q, want %q", got.BlobID.ValueString(), "newblob")
	}
}
