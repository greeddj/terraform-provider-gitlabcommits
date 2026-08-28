// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

func runDelete(t *testing.T, client *gitlab.Client, state filesResourceModel) *resource.DeleteResponse {
	t.Helper()
	return runDeleteOn(t, newTestResource(client), state)
}

func runDeleteOn(t *testing.T, res *filesResource, state filesResourceModel) *resource.DeleteResponse {
	t.Helper()
	ctx := t.Context()

	sresp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, sresp)
	sch := sresp.Schema

	st := tfsdk.State{Schema: sch}
	if d := st.Set(ctx, &state); d.HasError() {
		t.Fatalf("state.Set: %v", d)
	}
	resp := &resource.DeleteResponse{State: st}
	res.Delete(ctx, resource.DeleteRequest{State: st}, resp)
	return resp
}

func runUpdate(t *testing.T, client *gitlab.Client, plan, state filesResourceModel) *resource.UpdateResponse {
	t.Helper()
	return runUpdateOn(t, newTestResource(client), plan, state)
}

func runUpdateOn(t *testing.T, res *filesResource, plan, state filesResourceModel) *resource.UpdateResponse {
	t.Helper()
	req, resp := updateRequest(t, res, plan, state)
	res.Update(t.Context(), req, resp)
	return resp
}

// updateRequest builds the plan/state pair for res.Update. Kept apart from
// runUpdateOn so concurrency tests build requests on the test goroutine
// (t.Fatalf must not run anywhere else) and only call Update from workers.
func updateRequest(t *testing.T, res *filesResource, plan, state filesResourceModel) (resource.UpdateRequest, *resource.UpdateResponse) {
	t.Helper()
	ctx := t.Context()

	sresp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, sresp)
	sch := sresp.Schema

	pl := tfsdk.Plan{Schema: sch}
	if d := pl.Set(ctx, &plan); d.HasError() {
		t.Fatalf("plan.Set: %v", d)
	}
	st := tfsdk.State{Schema: sch}
	if d := st.Set(ctx, &state); d.HasError() {
		t.Fatalf("state.Set: %v", d)
	}
	return resource.UpdateRequest{Plan: pl, State: st}, &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
}

func runCreate(t *testing.T, client *gitlab.Client, plan filesResourceModel) *resource.CreateResponse {
	t.Helper()
	return runCreateOn(t, newTestResource(client), plan)
}

func runCreateOn(t *testing.T, res *filesResource, plan filesResourceModel) *resource.CreateResponse {
	t.Helper()
	req, resp := createRequest(t, res, plan)
	res.Create(t.Context(), req, resp)
	return resp
}

// createRequest is the Create counterpart of updateRequest.
func createRequest(t *testing.T, res *filesResource, plan filesResourceModel) (resource.CreateRequest, *resource.CreateResponse) {
	t.Helper()
	ctx := t.Context()

	sresp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, sresp)
	sch := sresp.Schema

	pl := tfsdk.Plan{Schema: sch}
	if d := pl.Set(ctx, &plan); d.HasError() {
		t.Fatalf("plan.Set: %v", d)
	}
	return resource.CreateRequest{Plan: pl}, &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
}

// TestCreate_NullCommitBodyErrors pins the DOS-1 guard in Create: a 2xx
// JSON-null commit body must produce an error diagnostic, not a nil-deref panic.
func TestCreate_NullCommitBodyErrors(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		case r.Method == http.MethodHead:
			http.Error(w, "absent", http.StatusNotFound)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("null"))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	resp := runCreate(t, client, readState("oldblob"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for a null commit body on Create")
	}
}

// TestUpdate_NullCommitBodyErrors pins the DOS-1 guard in Update.
func TestUpdate_NullCommitBodyErrors(t *testing.T) {
	postCalled := false
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalled = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("null"))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	state := readState("oldblob")
	plan := readState("oldblob")
	pf := plan.Files["f.txt"]
	pf.Content = types.StringValue("changed")
	plan.Files["f.txt"] = pf

	resp := runUpdate(t, client, plan, state)
	if !postCalled {
		t.Fatal("expected Update to attempt a commit when content changed")
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for a null commit body on Update")
	}
}

// TestDelete_DeleteOnDestroyFalseSkipsAPI: delete_on_destroy=false is a
// state-only drop and must not touch the API.
func TestDelete_DeleteOnDestroyFalseSkipsAPI(t *testing.T) {
	called := false
	client := newReadClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	s := readState("blob")
	s.DeleteOnDestroy = types.BoolValue(false)

	resp := runDelete(t, client, s)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if called {
		t.Error("expected no API calls when delete_on_destroy is false")
	}
}

// TestDelete_SkipsAbsentFiles: a file already gone at the remote is skipped, so
// no commit is produced (destroy is idempotent against out-of-band cleanup).
func TestDelete_SkipsAbsentFiles(t *testing.T) {
	postCalled := false
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalled = true
		}
		if r.Method == http.MethodHead {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	resp := runDelete(t, client, readState("blob"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if postCalled {
		t.Error("expected no commit when every managed file is already absent")
	}
}

// TestDelete_CommitsWhenPresent: present files produce exactly one delete commit.
func TestDelete_CommitsWhenPresent(t *testing.T) {
	postCalled := false
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("X-Gitlab-Blob-Id", "blob")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "main")
			w.Header().Set("X-Gitlab-Size", "3")
			w.WriteHeader(http.StatusOK)
		case http.MethodPost:
			postCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"delsha"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	resp := runDelete(t, client, readState("blob"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if !postCalled {
		t.Error("expected a delete commit when files are present")
	}
}

// TestUpdate_NoOpProducesNoCommit: when plan equals state, Update must make no
// API call (the one-commit-per-apply invariant produces zero commits here).
func TestUpdate_NoOpProducesNoCommit(t *testing.T) {
	called := false
	client := newReadClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	st := readState("blob")

	resp := runUpdate(t, client, st, st)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if called {
		t.Error("expected no API calls for a no-op update")
	}
}

// TestDelete_ProbeErrorFailsLoudly: a non-404 probe failure (revoked token,
// 5xx, timeout) must fail the destroy instead of skipping the file - an
// error-free Delete makes the framework drop the resource from state while
// the files may still exist in the repository.
func TestDelete_ProbeErrorFailsLoudly(t *testing.T) {
	postCalled := false
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalled = true
		}
		if r.Method == http.MethodHead {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	resp := runDelete(t, client, readState("blob"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected destroy to fail when the existence probe errors")
	}
	if postCalled {
		t.Error("expected no commit attempt after a failed probe")
	}
}

// TestDelete_PartialProbeErrorFailsLoudly: one 404 (legitimately absent) plus
// one failing probe must still abort the destroy - a partial failure would
// otherwise silently omit the failing path from the destroy commit.
func TestDelete_PartialProbeErrorFailsLoudly(t *testing.T) {
	postCalled := false
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalled = true
		}
		if r.Method == http.MethodHead {
			if strings.Contains(r.URL.Path, "gone.txt") {
				http.Error(w, "absent", http.StatusNotFound)
			} else {
				http.Error(w, "forbidden", http.StatusForbidden)
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	state := readState("blob")
	state.Files["gone.txt"] = fileModel{
		Content:         types.StringValue("x"),
		ContentBase64:   types.StringNull(),
		BlobID:          types.StringValue("blob2"),
		LastCommitID:    types.StringValue("lcid2"),
		ExecuteFilemode: types.BoolValue(false),
	}

	resp := runDelete(t, client, state)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected destroy to fail when any existence probe errors")
	}
	if postCalled {
		t.Error("expected no commit attempt after a failed probe")
	}
}

// TestCreate_EmptyRepositoryDiagnostics: on a project with zero commits every
// branch lookup 404s and no ref exists to branch from, so the usual "set
// create_branch_from" advice is a dead end. Create must say what actually
// helps, with or without create_branch_from configured.
func TestCreate_EmptyRepositoryDiagnostics(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			http.Error(w, "no branch", http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"empty_repo":true}`))
		default:
			http.Error(w, "unexpected call", http.StatusInternalServerError)
		}
	})

	for _, withFrom := range []bool{false, true} {
		plan := readState("blob")
		if withFrom {
			plan.CreateBranchFrom = types.StringValue("main")
		}
		resp := runCreate(t, client, plan)
		if !resp.Diagnostics.HasError() {
			t.Fatalf("withFrom=%v: expected an error on an empty repository", withFrom)
		}
		found := false
		for _, d := range resp.Diagnostics.Errors() {
			if strings.Contains(d.Detail(), "no commits") || strings.Contains(d.Summary(), "no commits") {
				found = true
			}
		}
		if !found {
			t.Errorf("withFrom=%v: diagnostic must explain the repository has no commits, got: %v", withFrom, resp.Diagnostics.Errors())
		}
	}
}

// TestCreate_HappyPathStampsState: a clean create must land one commit and
// stamp commit_sha, id, blob_id, and last_commit_id in state from the fake
// server's responses.
func TestCreate_HappyPathStampsState(t *testing.T) {
	var commitBody string
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "main":
			// Adopt probe before the commit: path does not exist yet.
			http.Error(w, "absent", http.StatusNotFound)
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "newsha":
			// stampBlobs probe at the created commit.
			w.Header().Set("X-Gitlab-Blob-Id", "stampedblob")
			w.Header().Set("X-Gitlab-Last-Commit-Id", "newsha")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "newsha")
			w.Header().Set("X-Gitlab-Size", "3")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			commitBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"newsha"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	resp := runCreate(t, client, readState("ignored"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if !strings.Contains(commitBody, `"action":"create"`) {
		t.Errorf("commit body must carry a create action, got: %s", commitBody)
	}

	var out filesResourceModel
	if d := resp.State.Get(t.Context(), &out); d.HasError() {
		t.Fatalf("state.Get: %v", d)
	}
	if out.CommitSHA.ValueString() != "newsha" {
		t.Errorf("commit_sha = %q, want %q", out.CommitSHA.ValueString(), "newsha")
	}
	if out.ID.ValueString() != "proj::main" {
		t.Errorf("id = %q, want %q", out.ID.ValueString(), "proj::main")
	}
	f := out.Files["f.txt"]
	if f.BlobID.ValueString() != "stampedblob" || f.LastCommitID.ValueString() != "newsha" {
		t.Errorf("stamped blob/lcid = %q/%q, want stampedblob/newsha", f.BlobID.ValueString(), f.LastCommitID.ValueString())
	}
}

// TestCreate_AdoptRewritesToUpdateEndToEnd: Create's own adopt branch (not the
// diffActions copy) must rewrite create into update and forward the probed
// lock token into the commit body.
func TestCreate_AdoptRewritesToUpdateEndToEnd(t *testing.T) {
	var commitBody string
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "main":
			// Adopt probe: the path already exists remotely.
			w.Header().Set("X-Gitlab-Blob-Id", "remoteblob")
			w.Header().Set("X-Gitlab-Last-Commit-Id", "adopt-lcid")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "main")
			w.Header().Set("X-Gitlab-Size", "3")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "adsha":
			w.Header().Set("X-Gitlab-Blob-Id", "stampedblob")
			w.Header().Set("X-Gitlab-Last-Commit-Id", "adsha")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "adsha")
			w.Header().Set("X-Gitlab-Size", "3")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			commitBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"adsha"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	resp := runCreate(t, client, readState("ignored"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if !strings.Contains(commitBody, `"action":"update"`) {
		t.Errorf("adopt must rewrite create into update, body: %s", commitBody)
	}
	if !strings.Contains(commitBody, `"last_commit_id":"adopt-lcid"`) {
		t.Errorf("adopt-update must carry the probed lock token, body: %s", commitBody)
	}
}

// TestUpdate_HappyPathStampsState: a content change pushes one commit and
// restamps computed fields from the created commit.
func TestUpdate_HappyPathStampsState(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "upsha":
			w.Header().Set("X-Gitlab-Blob-Id", "upblob")
			w.Header().Set("X-Gitlab-Last-Commit-Id", "upsha")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "upsha")
			w.Header().Set("X-Gitlab-Size", "7")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"upsha"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	state := readState("oldblob")
	plan := readState("oldblob")
	pf := plan.Files["f.txt"]
	pf.Content = types.StringValue("changed")
	plan.Files["f.txt"] = pf

	resp := runUpdate(t, client, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	var out filesResourceModel
	if d := resp.State.Get(t.Context(), &out); d.HasError() {
		t.Fatalf("state.Get: %v", d)
	}
	if out.CommitSHA.ValueString() != "upsha" {
		t.Errorf("commit_sha = %q, want %q", out.CommitSHA.ValueString(), "upsha")
	}
	f := out.Files["f.txt"]
	if f.BlobID.ValueString() != "upblob" || f.LastCommitID.ValueString() != "upsha" {
		t.Errorf("stamped blob/lcid = %q/%q, want upblob/upsha", f.BlobID.ValueString(), f.LastCommitID.ValueString())
	}
}

// TestBranchHelpers drives the branch helpers directly: existence check
// outcomes and the missing-branch preflight.
func TestBranchHelpers(t *testing.T) {
	t.Run("branchExists true", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		})
		r := newTestResource(client)
		ok, err := r.branchExists(t.Context(), "proj", "main")
		if err != nil || !ok {
			t.Fatalf("want (true, nil), got (%v, %v)", ok, err)
		}
	})

	t.Run("branchExists false on 404", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no branch", http.StatusNotFound)
		})
		r := newTestResource(client)
		ok, err := r.branchExists(t.Context(), "proj", "feature")
		if err != nil || ok {
			t.Fatalf("want (false, nil), got (%v, %v)", ok, err)
		}
	})

	t.Run("branchExists surfaces non-404", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		})
		r := newTestResource(client)
		_, err := r.branchExists(t.Context(), "proj", "main")
		if err == nil || !strings.Contains(err.Error(), "checking branch") {
			t.Fatalf("want a checking-branch error, got: %v", err)
		}
	})

	t.Run("preflight rejects empty repository", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"empty_repo":true}`))
		})
		r := newTestResource(client)
		err := r.missingBranchPreflight(t.Context(), "proj", "feature", "main")
		if err == nil || !strings.Contains(err.Error(), "no commits") {
			t.Fatalf("want the empty-repository diagnostic, got: %v", err)
		}
	})

	t.Run("preflight demands create_branch_from", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
		})
		r := newTestResource(client)
		err := r.missingBranchPreflight(t.Context(), "proj", "feature", "")
		if err == nil || !strings.Contains(err.Error(), "create_branch_from") {
			t.Fatalf("want the create_branch_from hint, got: %v", err)
		}
	})

}

// TestCreate_StartBranchRejectionNamesRef: when GitLab rejects the first
// commit that was to materialise the branch, the diagnostic names the branch
// and the create_branch_from ref, since that pairing is the usual culprit.
func TestCreate_StartBranchRejectionNamesRef(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			http.Error(w, "no branch yet", http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
		case r.Method == http.MethodHead:
			http.Error(w, "absent", http.StatusNotFound)
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"You can only create or edit files when you are on a branch"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	plan := readState("ignored")
	plan.Branch = types.StringValue("feature")
	plan.ID = types.StringValue("proj::feature")
	plan.CreateBranchFrom = types.StringValue("nope")

	resp := runCreate(t, client, plan)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected the rejected first commit to fail Create")
	}
	summary := resp.Diagnostics.Errors()[0].Summary()
	if !strings.Contains(summary, `creating branch "feature" from create_branch_from ref "nope"`) {
		t.Errorf("summary must name the branch and ref, got: %q", summary)
	}
}

// TestCreate_AdoptsFromCreateBranchFromRef is the regression test for
// adoption across branch materialisation: when the branch does not exist yet
// and create_branch_from points at a ref that already contains a managed
// path, the adopt probe must resolve against that source ref - the new
// branch inherits the file, so a plain create would die with "already
// exists". The branch itself is materialised by start_branch on the commit,
// never by a separate branch-creation call.
func TestCreate_AdoptsFromCreateBranchFromRef(t *testing.T) {
	var commitBody string
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			http.Error(w, "no branch yet", http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "main":
			// The managed path already exists on the source ref.
			w.Header().Set("X-Gitlab-Blob-Id", "srcblob")
			w.Header().Set("X-Gitlab-Last-Commit-Id", "src-lcid")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "main")
			w.Header().Set("X-Gitlab-Size", "3")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "absha":
			w.Header().Set("X-Gitlab-Blob-Id", "stampedblob")
			w.Header().Set("X-Gitlab-Last-Commit-Id", "absha")
			w.Header().Set("X-Gitlab-File-Path", "f.txt")
			w.Header().Set("X-Gitlab-Ref", "absha")
			w.Header().Set("X-Gitlab-Size", "3")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/branches"):
			t.Error("the branch must be created by start_branch on the commit, not by a separate call")
			http.Error(w, "unexpected", http.StatusInternalServerError)
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			commitBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"absha"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	plan := readState("ignored")
	plan.Branch = types.StringValue("feature")
	plan.ID = types.StringValue("proj::feature")
	plan.CreateBranchFrom = types.StringValue("main")

	resp := runCreate(t, client, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if !strings.Contains(commitBody, `"start_branch":"main"`) {
		t.Errorf("the first commit must materialise the branch via start_branch, body: %s", commitBody)
	}
	if !strings.Contains(commitBody, `"action":"update"`) {
		t.Errorf("inherited path must be adopted as an update, body: %s", commitBody)
	}
	if !strings.Contains(commitBody, `"last_commit_id":"src-lcid"`) {
		t.Errorf("adopt-update must carry the source ref's lock token, body: %s", commitBody)
	}
}

// TestCommitOptions_AuthorPropagation: author overrides reach the commit
// options only when set.
func TestCommitOptions_AuthorPropagation(t *testing.T) {
	m := filesResourceModel{
		Branch:        types.StringValue("main"),
		CommitMessage: types.StringValue("msg"),
		AuthorEmail:   types.StringValue("a@b.c"),
		AuthorName:    types.StringValue("Author"),
	}
	opts := commitOptions(m, nil)
	if opts.AuthorEmail == nil || *opts.AuthorEmail != "a@b.c" || opts.AuthorName == nil || *opts.AuthorName != "Author" {
		t.Errorf("author fields must propagate, got %+v", opts)
	}

	m.AuthorEmail = types.StringNull()
	m.AuthorName = types.StringNull()
	opts = commitOptions(m, nil)
	if opts.AuthorEmail != nil || opts.AuthorName != nil {
		t.Error("null author fields must stay unset in commit options")
	}
}

// TestCreate_ExistingBranchSendsNoStartBranch: start_branch is only for
// materialising a missing branch; on an existing branch GitLab would reject
// it ("A branch called ... already exists").
func TestCreate_ExistingBranchSendsNoStartBranch(t *testing.T) {
	var commitBody string
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "main":
			http.Error(w, "absent", http.StatusNotFound)
		case r.Method == http.MethodHead:
			stampHeaders(w, "newsha")
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			commitBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"newsha"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	plan := readState("ignored")
	plan.CreateBranchFrom = types.StringValue("main")
	if resp := runCreate(t, client, plan); resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if strings.Contains(commitBody, "start_branch") {
		t.Errorf("start_branch must not be sent when the branch exists, body: %s", commitBody)
	}
}

// stampHeaders answers a metadata probe (HEAD) with the fields stampBlobs
// reads after a commit.
func stampHeaders(w http.ResponseWriter, commitSHA string) {
	w.Header().Set("X-Gitlab-Blob-Id", "blob-"+commitSHA)
	w.Header().Set("X-Gitlab-Last-Commit-Id", commitSHA)
	w.Header().Set("X-Gitlab-File-Path", "f.txt")
	w.Header().Set("X-Gitlab-Ref", commitSHA)
	w.Header().Set("X-Gitlab-Size", "7")
	w.WriteHeader(http.StatusOK)
}

// changedPlan returns a plan/state pair whose only difference is the content
// of f.txt, i.e. exactly one update action.
func changedPlan() (plan, state filesResourceModel) {
	state = readState("oldblob")
	plan = readState("oldblob")
	pf := plan.Files["f.txt"]
	pf.Content = types.StringValue("changed")
	plan.Files["f.txt"] = pf
	return plan, state
}

// TestCommitRetryPolicy pins which failures may replay the commit POST: rate
// limiting and connection failures that happen before the request is sent,
// nothing else - a replay after GitLab already landed the commit would be a
// second commit for one apply.
func TestCommitRetryPolicy(t *testing.T) {
	cases := []struct {
		err   error
		resp  *http.Response
		name  string
		retry bool
	}{
		{name: "429", resp: &http.Response{StatusCode: http.StatusTooManyRequests}, retry: true},
		{name: "503", resp: &http.Response{StatusCode: http.StatusServiceUnavailable}, retry: false},
		{name: "502", resp: &http.Response{StatusCode: http.StatusBadGateway}, retry: false},
		{name: "201", resp: &http.Response{StatusCode: http.StatusCreated}, retry: false},
		{name: "dial refused", err: &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, retry: true},
		{name: "read reset", err: &url.Error{Op: "Post", Err: &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}}, retry: false},
		// net/dial wraps a resolver failure as OpError{Op: "dial"} around the
		// DNSError, so the DNS check must win over the dial check.
		{name: "dns temporary", err: &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "timeout", IsTemporary: true}}}, retry: true},
		{name: "dns nxdomain", err: &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}}, retry: false},
		{name: "tls handshake timeout", err: &url.Error{Op: "Post", Err: errors.New("net/http: TLS handshake timeout")}, retry: true},
		{name: "unexpected eof", err: &url.Error{Op: "Post", Err: io.ErrUnexpectedEOF}, retry: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := commitRetryPolicy(t.Context(), c.resp, c.err)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.retry {
				t.Errorf("retry = %v, want %v", got, c.retry)
			}
		})
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := commitRetryPolicy(ctx, &http.Response{StatusCode: http.StatusTooManyRequests}, nil); got || !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled ctx must stop retrying, got (%v, %v)", got, err)
	}
}

// TestUpdate_CommitIsNotRetriedOn5xx: with retries enabled, a 5xx on the
// commit POST is surfaced after exactly one attempt.
func TestUpdate_CommitIsNotRetriedOn5xx(t *testing.T) {
	var posts atomic.Int32
	res := newRetryingResource(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			posts.Add(1)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		case http.MethodHead:
			stampHeaders(w, "upsha")
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	plan, state := changedPlan()
	resp := runUpdateOn(t, res, plan, state)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected the 502 to fail the update")
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("commit POST attempts = %d, want exactly 1 (a replay could land a second commit)", got)
	}
	d := resp.Diagnostics.Errors()[0]
	if !strings.Contains(d.Summary(), "HTTP 502") || !strings.Contains(d.Detail(), "terraform plan") {
		t.Errorf("diagnostic must carry the status and the reconcile advice, got: %s / %s", d.Summary(), d.Detail())
	}
}

// TestUpdate_CommitIsRetriedOn429: rate limiting rejects the request before
// it is processed, so a retry is safe and expected.
func TestUpdate_CommitIsRetriedOn429(t *testing.T) {
	var posts atomic.Int32
	res := newRetryingResource(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if posts.Add(1) == 1 {
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"upsha"}`))
		case http.MethodHead:
			stampHeaders(w, "upsha")
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	plan, state := changedPlan()
	resp := runUpdateOn(t, res, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if got := posts.Load(); got != 2 {
		t.Errorf("commit POST attempts = %d, want 2 (one 429, one success)", got)
	}
}

// TestUpdate_CommitHonoursDisabledRetries: with max_retries = 0 the commit
// request must not get a retry policy of its own, or the per-request policy
// would bypass WithoutRetries and replay a 429 anyway.
func TestUpdate_CommitHonoursDisabledRetries(t *testing.T) {
	var posts atomic.Int32
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	plan, state := changedPlan()
	resp := runUpdate(t, client, plan, state)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected the 429 to fail the update when retries are disabled")
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("commit POST attempts = %d, want exactly 1 with retries disabled", got)
	}
}

// TestBranchLocks_SerialisesPerBranch: a held (project, branch) lock blocks a
// second acquire until released, honours ctx while waiting, and leaves other
// branches independent.
func TestBranchLocks_SerialisesPerBranch(t *testing.T) {
	locks := newBranchLocks()
	release, err := locks.acquire(t.Context(), "proj", "main")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, waitErr := locks.acquire(ctx, "proj", "main"); !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("acquire on a held lock must return the ctx error, got %v", waitErr)
	}

	otherRelease, err := locks.acquire(t.Context(), "proj", "other")
	if err != nil {
		t.Fatalf("a different branch must not be held back: %v", err)
	}
	otherRelease()

	// release is idempotent: call sites defer it and also call it early.
	release()
	release()
	again, err := locks.acquire(t.Context(), "proj", "main")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	again()
}

// TestUpdate_ConcurrentSameBranchCommitsAreSerialised: resource instances
// sharing one branch (the for_each layout under terraform -parallelism) must
// never have two commit POSTs in flight at once - that is the ref race GitLab
// rejects with HTTP 400 "reference does not point to expected object".
func TestUpdate_ConcurrentSameBranchCommitsAreSerialised(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			n := inFlight.Add(1)
			for {
				m := maxInFlight.Load()
				if n <= m || maxInFlight.CompareAndSwap(m, n) {
					break
				}
			}
			// Long enough that unserialised goroutines provably overlap.
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sha"}`))
		case http.MethodHead:
			stampHeaders(w, "sha")
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	locks := newBranchLocks()

	var wg sync.WaitGroup
	for range 4 {
		// One filesResource per instance, sharing the provider's locks,
		// exactly as Configure wires them.
		res := &filesResource{client: client, locks: locks}
		plan, state := changedPlan()
		req, resp := updateRequest(t, res, plan, state)
		wg.Go(func() {
			res.Update(t.Context(), req, resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("unexpected error: %v", resp.Diagnostics.Errors())
			}
		})
	}
	wg.Wait()
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("commits to one branch overlapped: max in flight = %d, want 1", got)
	}
}

// TestCreate_ConcurrentBranchMaterialisationIsSerialised: several instances
// creating on the same missing branch must not all try to materialise it.
// Create holds the branch lock from the existence check through the commit,
// so the first instance creates the branch with start_branch and the others
// then see it and commit plainly.
func TestCreate_ConcurrentBranchMaterialisationIsSerialised(t *testing.T) {
	var branchCreated atomic.Bool
	var startBranchCommits, commits atomic.Int32
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			if !branchCreated.Load() {
				http.Error(w, "no branch yet", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"feature","commit":{"id":"base"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "sha":
			stampHeaders(w, "sha")
		case r.Method == http.MethodHead:
			http.Error(w, "absent", http.StatusNotFound)
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			commits.Add(1)
			if strings.Contains(string(b), `"start_branch"`) {
				if branchCreated.Swap(true) {
					t.Error("start_branch sent for a branch that already exists")
				}
				startBranchCommits.Add(1)
			} else if !branchCreated.Load() {
				t.Error("plain commit attempted before the branch was materialised")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sha"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	locks := newBranchLocks()

	var wg sync.WaitGroup
	for range 4 {
		res := &filesResource{client: client, locks: locks}
		plan := readState("ignored")
		plan.Branch = types.StringValue("feature")
		plan.ID = types.StringValue("proj::feature")
		plan.CreateBranchFrom = types.StringValue("main")
		req, resp := createRequest(t, res, plan)
		wg.Go(func() {
			res.Create(t.Context(), req, resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("unexpected error: %v", resp.Diagnostics.Errors())
			}
		})
	}
	wg.Wait()
	if got := startBranchCommits.Load(); got != 1 {
		t.Errorf("start_branch commits = %d, want exactly 1", got)
	}
	if got := commits.Load(); got != 4 {
		t.Errorf("commits = %d, want one per instance", got)
	}
}

// TestCreate_CommitSHASourceUsesStartSHA: the commits API separates
// start_branch from start_sha, so a create_branch_from that is a full commit
// SHA must travel as start_sha.
func TestCreate_CommitSHASourceUsesStartSHA(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	var commitBody string
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			http.Error(w, "no branch yet", http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "newsha":
			stampHeaders(w, "newsha")
		case r.Method == http.MethodHead:
			http.Error(w, "absent", http.StatusNotFound)
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			commitBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"newsha"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	plan := readState("ignored")
	plan.Branch = types.StringValue("feature")
	plan.ID = types.StringValue("proj::feature")
	plan.CreateBranchFrom = types.StringValue(sha)
	if resp := runCreate(t, client, plan); resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if !strings.Contains(commitBody, `"start_sha":"`+sha+`"`) || strings.Contains(commitBody, "start_branch") {
		t.Errorf("a SHA source must be sent as start_sha only, body: %s", commitBody)
	}
}

func TestIsCommitSHA(t *testing.T) {
	cases := map[string]bool{
		"main": false,
		"0123456789abcdef0123456789abcdef01234567":                         true,
		"0123456789ABCDEF0123456789abcdef01234567":                         true,
		"0123456789abcdef0123456789abcdef0123456":                          false,
		"0123456789abcdef0123456789abcdef0123456g":                         false,
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": true,
	}
	for in, want := range cases {
		if got := isCommitSHA(in); got != want {
			t.Errorf("isCommitSHA(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestCreate_CommitIsNotRetriedOn5xx and TestDelete_CommitIsNotRetriedOn5xx
// pin the retry policy on the two commit sites the Update tests do not reach.
func TestCreate_CommitIsNotRetriedOn5xx(t *testing.T) {
	var posts atomic.Int32
	res := newRetryingResource(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		case r.Method == http.MethodHead:
			http.Error(w, "absent", http.StatusNotFound)
		case r.Method == http.MethodPost:
			posts.Add(1)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	resp := runCreateOn(t, res, readState("ignored"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected the 502 to fail the create")
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("commit POST attempts = %d, want exactly 1", got)
	}
}

func TestDelete_CommitIsNotRetriedOn5xx(t *testing.T) {
	var posts atomic.Int32
	res := newRetryingResource(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			stampHeaders(w, "sha")
		case http.MethodPost:
			posts.Add(1)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	resp := runDeleteOn(t, res, readState("blob"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected the 502 to fail the destroy")
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("commit POST attempts = %d, want exactly 1", got)
	}
}
