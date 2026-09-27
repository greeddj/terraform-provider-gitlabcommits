// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
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
// response and (on success) the resulting state model. The response starts
// as a copy of the current state, as fwserver seeds it, so a Read that means
// to remove the resource has to say so.
func runRead(t *testing.T, client *gitlab.Client, state filesResourceModel) (*resource.ReadResponse, filesResourceModel) {
	t.Helper()
	ctx := t.Context()
	res := newTestResource(client)

	sresp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, sresp)
	sch := sresp.Schema

	st := tfsdk.State{Schema: sch}
	if d := st.Set(ctx, &state); d.HasError() {
		t.Fatalf("state.Set: %v", d)
	}

	req := resource.ReadRequest{State: st}
	resp := &resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: st.Raw.Copy()}}
	res.Read(ctx, req, resp)

	var out filesResourceModel
	if !resp.Diagnostics.HasError() {
		if !resp.State.Raw.IsFullyKnown() {
			t.Errorf("Read returned state with unknown values: %s", resp.State.Raw)
		}
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

// newTestResource mirrors what Configure wires for max_retries = 0: the
// client, fresh branch locks, and no commit retry policy.
func newTestResource(client *gitlab.Client) *filesResource {
	return &filesResource{client: client, locks: newBranchLocks()}
}

// newRetryingResource mirrors the max_retries > 0 wiring: a client that
// retries (bounded, with a millisecond backoff so a faked 429 does not stall
// the test) and the commit-specific retry policy on the commit request.
func newRetryingResource(t *testing.T, h http.HandlerFunc) *filesResource {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client, err := gitlab.NewClient("tok", gitlab.WithBaseURL(srv.URL+"/"),
		gitlab.WithCustomRetryMax(2),
		gitlab.WithCustomRetryWaitMinMax(time.Millisecond, 2*time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return &filesResource{client: client, locks: newBranchLocks(), retryCommits: true}
}

// TestRead_DropsMissingFile covers the drift drop-pass: a managed file that
// 404s on the metadata probe is removed from state so the next plan recreates
// it. The branch itself still exists, so the resource stays in state.
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

// TestRead_EmptyFilesChecksBranch: right after import there is nothing to
// probe, so the branch is the only thing Read can verify - gone means the
// resource is removed, present means state passes through.
func TestRead_EmptyFilesChecksBranch(t *testing.T) {
	for _, branchStatus := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(branchStatus), func(t *testing.T) {
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/") {
					if branchStatus == http.StatusOK {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"head"}}`))
						return
					}
					http.Error(w, "gone", branchStatus)
					return
				}
				t.Errorf("unexpected call %s %s with no managed files", r.Method, r.URL.Path)
			})
			state := readState("blob")
			state.Files = map[string]fileModel{}

			resp, _ := runRead(t, client, state)
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			if gone := resp.State.Raw.IsNull(); gone != (branchStatus == http.StatusNotFound) {
				t.Fatalf("resource removed = %v, want %v", gone, branchStatus == http.StatusNotFound)
			}
		})
	}
}

// TestRead_TwoFileProbeOutcomes pins how per-file probe outcomes combine: a
// 404 drops only its own path, and only once the branch lookup has answered
// (a Gitaly failure reaches the Files API as a 404 but fails the branch
// lookup, which fails the refresh with nothing dropped); any other probe
// failure fails the refresh with nothing dropped; and two 404s on a live
// branch empty the files map but keep the resource.
func TestRead_TwoFileProbeOutcomes(t *testing.T) {
	cases := []struct {
		status       map[string]int
		name         string
		wantErr      string
		wantFiles    []string
		branchStatus int
		branchGets   int32
	}{
		{name: "one gone", status: map[string]int{"a.txt": http.StatusNotFound, "f.txt": http.StatusOK}, wantFiles: []string{"f.txt"}, branchGets: 1},
		{
			name: "one gone while the branch lookup fails", status: map[string]int{"a.txt": http.StatusNotFound, "f.txt": http.StatusOK},
			branchStatus: http.StatusInternalServerError, wantErr: "Gitaly times out", branchGets: 1,
		},
		{
			name: "one gone on a branch that answers 404", status: map[string]int{"a.txt": http.StatusNotFound, "f.txt": http.StatusOK},
			branchStatus: http.StatusNotFound, wantFiles: []string{"f.txt"}, branchGets: 1,
		},
		{name: "one forbidden", status: map[string]int{"a.txt": http.StatusForbidden, "f.txt": http.StatusOK}, wantErr: "HTTP 403"},
		{name: "one gone one forbidden", status: map[string]int{"a.txt": http.StatusNotFound, "f.txt": http.StatusForbidden}, wantErr: "HTTP 403"},
		{name: "both gone on a live branch", status: map[string]int{"a.txt": http.StatusNotFound, "f.txt": http.StatusNotFound}, wantFiles: []string{}, branchGets: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var branchGets atomic.Int32
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/") {
					branchGets.Add(1)
					if tc.branchStatus != 0 {
						http.Error(w, http.StatusText(tc.branchStatus), tc.branchStatus)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"head"}}`))
					return
				}
				for p, status := range tc.status {
					if r.Method != http.MethodHead || !strings.HasSuffix(r.URL.Path, "/"+p) {
						continue
					}
					if status == http.StatusOK {
						metaHeaders(w, "oldblob", "oldlcid", false)
					} else {
						http.Error(w, http.StatusText(status), status)
					}
					return
				}
				t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			})
			state := readState("oldblob")
			state.Files["a.txt"] = state.Files["f.txt"]

			resp, out := runRead(t, client, state)
			if got := branchGets.Load(); got != tc.branchGets {
				t.Errorf("branch lookups = %d, want %d", got, tc.branchGets)
			}
			if tc.wantErr != "" {
				wantDiag(t, "error", resp.Diagnostics.Errors(), tc.wantErr)
				var kept filesResourceModel
				if resp.State.Raw.IsNull() || resp.State.Get(t.Context(), &kept).HasError() || len(kept.Files) != 2 {
					t.Errorf("a failed refresh must drop nothing, state: %s", resp.State.Raw)
				}
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			if resp.State.Raw.IsNull() {
				t.Fatal("the resource must stay in state")
			}
			if got := slices.Sorted(maps.Keys(out.Files)); !slices.Equal(got, tc.wantFiles) {
				t.Errorf("files in state = %v, want %v", got, tc.wantFiles)
			}
			for p, f := range out.Files {
				if f.Content.ValueString() != "old" || f.BlobID.ValueString() != "oldblob" || f.LastCommitID.ValueString() != "oldlcid" {
					t.Errorf("kept path %q must keep its values, got %q/%q/%q", p, f.Content.ValueString(), f.BlobID.ValueString(), f.LastCommitID.ValueString())
				}
			}
		})
	}
}

// TestRead_DriftedBlobFetchFailureFails: once the probe reports a new blob,
// a failed content fetch must fail the refresh. Treating it as unchanged
// would keep the old content in state while GitLab holds different bytes.
func TestRead_DriftedBlobFetchFailureFails(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			metaHeaders(w, "newblob", "newlcid", false)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	resp, _ := runRead(t, client, readState("oldblob"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a failed content fetch for a drifted blob to fail the refresh")
	}
}

// TestRead_EmptyBlobIDErrors: a metadata response without X-Gitlab-Blob-Id
// would compare equal to nothing forever; it is an error, not "unchanged".
func TestRead_EmptyBlobIDErrors(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	resp, _ := runRead(t, client, readState("oldblob"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for a metadata response without blob_id")
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

// TestRead_NullFileBodyOnDriftErrors: a 2xx JSON-null GetFile body for a
// drifted blob must surface an error, not be silently treated as unchanged.
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

// TestRead_OversizedBlobIDIgnored: an absurdly long blob_id from GetFile is
// not persisted; state keeps blob_id null, takes last_commit_id from the
// metadata response, and a warning is emitted.
func TestRead_OversizedBlobIDIgnored(t *testing.T) {
	oversized := strings.Repeat("a", maxBlobIDLen+1)
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			metaHeaders(w, "newblob", "metalcid", false)
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
	if f := out.Files["f.txt"]; !f.BlobID.IsNull() || f.LastCommitID.ValueString() != "metalcid" || f.Content.ValueString() != "new" {
		t.Errorf("blob_id / last_commit_id / content = %s / %q / %q, want null / metalcid / new",
			f.BlobID, f.LastCommitID.ValueString(), f.Content.ValueString())
	}
}

// utf16Drift is "a" in UTF-16LE with its byte order mark, as PowerShell 5
// writes it: bytes that are not valid UTF-8.
var utf16Drift = []byte{0xff, 0xfe, 0x61, 0x00}

// utf16DriftFake holds f.txt drifted to utf16Drift in commit newlcid.
func utf16DriftFake() *repoFake {
	return &repoFake{files: map[string]string{"f.txt": "newlcid"}, content: map[string][]byte{"f.txt": utf16Drift}}
}

// TestRead_TextMisfitDriftRecordedAsBase64: a file managed through the text
// content attribute that drifts to invalid UTF-8 cannot be held there (cty
// would silently turn the bytes into U+FFFD), and Read cannot see whether
// the configuration has switched to content_base64. So the bytes land in
// content_base64 with content null and a warning, and the refresh succeeds
// with the new ids.
func TestRead_TextMisfitDriftRecordedAsBase64(t *testing.T) {
	resp, out := runRead(t, utf16DriftFake().client(t), readState("oldblob"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	wantDiag(t, "warning", resp.Diagnostics.Warnings(), `file "f.txt" is managed through `+"`content`")
	if w := resp.Diagnostics.Warnings(); len(w) == 1 && !strings.Contains(w[0].Detail(), "manage the file through `content_base64`") {
		t.Errorf("the warning must say how to keep the new bytes, got: %s", w[0].Detail())
	}
	f := out.Files["f.txt"]
	if !f.Content.IsNull() {
		t.Errorf("content = %q, want null", f.Content.ValueString())
	}
	if want := base64.StdEncoding.EncodeToString(utf16Drift); f.ContentBase64.ValueString() != want {
		t.Errorf("content_base64 = %q, want the remote bytes %q", f.ContentBase64.ValueString(), want)
	}
	if f.BlobID.ValueString() != "blob-newlcid" || f.LastCommitID.ValueString() != "newlcid" {
		t.Errorf("blob_id/last_commit_id = %q/%q, want blob-newlcid/newlcid", f.BlobID.ValueString(), f.LastCommitID.ValueString())
	}
}

// TestTextMisfitDrift_Converges drives the refresh above into the next apply
// both ways the warning offers. With the configuration unchanged, the plan
// restores the text: one update, carrying the token that refresh read, so
// the lock passes. With the configuration switched to content_base64 of the
// new bytes, nothing is committed.
func TestTextMisfitDrift_Converges(t *testing.T) {
	refresh := func(t *testing.T, client *gitlab.Client) filesResourceModel {
		t.Helper()
		resp, refreshed := runRead(t, client, readState("oldblob"))
		if resp.Diagnostics.HasError() {
			t.Fatalf("refresh: %v", resp.Diagnostics.Errors())
		}
		return refreshed
	}

	t.Run("restore the configured text", func(t *testing.T) {
		fake := utf16DriftFake()
		client := fake.client(t)
		refreshed := refresh(t, client)

		resp := runUpdate(t, client, readState("oldblob"), refreshed)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
		}
		commits, _, _ := fake.recorded()
		if want := [][]string{{"update:f.txt@newlcid"}}; !slices.EqualFunc(commits, want, slices.Equal[[]string]) {
			t.Errorf("commits = %q, want %q", commits, want)
		}
		var out filesResourceModel
		if d := resp.State.Get(t.Context(), &out); d.HasError() {
			t.Fatalf("state.Get: %v", d)
		}
		if f := out.Files["f.txt"]; f.Content.ValueString() != "old" || !f.ContentBase64.IsNull() {
			t.Errorf("state content/content_base64 = %s/%s, want \"old\"/null", f.Content, f.ContentBase64)
		}
	})

	t.Run("keep the new bytes", func(t *testing.T) {
		fake := utf16DriftFake()
		client := fake.client(t)
		refreshed := refresh(t, client)

		plan := readState("oldblob")
		f := plan.Files["f.txt"]
		f.Content = types.StringNull()
		f.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString(utf16Drift))
		plan.Files["f.txt"] = f
		resp := runUpdate(t, client, plan, refreshed)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
		}
		if commits, _, _ := fake.recorded(); len(commits) != 0 {
			t.Errorf("commits = %q, want none", commits)
		}
	})
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

// TestRead_ExecBitOnlyDriftIsDetected: an out-of-band chmod leaves the blob
// alone, so the exec bit alone must send Read to fetch the file and restamp
// execute_filemode and last_commit_id from it, in either direction.
func TestRead_ExecBitOnlyDriftIsDetected(t *testing.T) {
	for _, remoteExec := range []bool{true, false} {
		t.Run(fmt.Sprintf("remote exec %v", remoteExec), func(t *testing.T) {
			var gets atomic.Int32
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					metaHeaders(w, "oldblob", "oldlcid", remoteExec)
					return
				}
				gets.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"file_path":"f.txt","blob_id":"oldblob","content":%q,"encoding":"base64","last_commit_id":"chmodlcid","execute_filemode":%v,"size":3}`,
					base64.StdEncoding.EncodeToString([]byte("old")), remoteExec)
			})
			state := readState("oldblob")
			f := state.Files["f.txt"]
			f.ExecuteFilemode = types.BoolValue(!remoteExec)
			state.Files["f.txt"] = f

			resp, out := runRead(t, client, state)
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			if got := gets.Load(); got != 1 {
				t.Errorf("GetFile calls = %d, want 1 for a flipped exec bit", got)
			}
			got := out.Files["f.txt"]
			if got.ExecuteFilemode.ValueBool() != remoteExec {
				t.Errorf("execute_filemode = %v, want the remote %v", got.ExecuteFilemode.ValueBool(), remoteExec)
			}
			if got.LastCommitID.ValueString() != "chmodlcid" {
				t.Errorf("last_commit_id = %q, want chmodlcid from the fetched file", got.LastCommitID.ValueString())
			}
			if got.Content.ValueString() != "old" || got.BlobID.ValueString() != "oldblob" {
				t.Errorf("content/blob_id = %q/%q, want them unchanged", got.Content.ValueString(), got.BlobID.ValueString())
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
