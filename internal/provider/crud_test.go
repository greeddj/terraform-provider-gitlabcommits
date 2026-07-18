// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

func runDelete(t *testing.T, client *gitlab.Client, state filesResourceModel) *resource.DeleteResponse {
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
	resp := &resource.DeleteResponse{State: st}
	res.Delete(ctx, resource.DeleteRequest{State: st}, resp)
	return resp
}

func runUpdate(t *testing.T, client *gitlab.Client, plan, state filesResourceModel) *resource.UpdateResponse {
	t.Helper()
	ctx := context.Background()
	res := &filesResource{client: client}

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
	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	res.Update(ctx, resource.UpdateRequest{Plan: pl, State: st}, resp)
	return resp
}

func runCreate(t *testing.T, client *gitlab.Client, plan filesResourceModel) *resource.CreateResponse {
	t.Helper()
	ctx := context.Background()
	res := &filesResource{client: client}

	sresp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, sresp)
	sch := sresp.Schema

	pl := tfsdk.Plan{Schema: sch}
	if d := pl.Set(ctx, &plan); d.HasError() {
		t.Fatalf("plan.Set: %v", d)
	}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: pl}, resp)
	return resp
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
	if d := resp.State.Get(context.Background(), &out); d.HasError() {
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
	if d := resp.State.Get(context.Background(), &out); d.HasError() {
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

// TestEnsureBranch drives every ensureBranch branch directly: present branch,
// non-404 lookup failure, absent branch without a source ref, successful
// creation with the right payload, and a failing creation.
func TestEnsureBranch(t *testing.T) {
	t.Run("branch exists", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		})
		r := &filesResource{client: client}
		if err := r.ensureBranch(context.Background(), "proj", "main", ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("non-404 lookup failure", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		})
		r := &filesResource{client: client}
		err := r.ensureBranch(context.Background(), "proj", "main", "")
		if err == nil || !strings.Contains(err.Error(), "checking branch") {
			t.Fatalf("want a checking-branch error, got: %v", err)
		}
	})

	t.Run("absent without create_branch_from", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/branches/") {
				http.Error(w, "no branch", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
		})
		r := &filesResource{client: client}
		err := r.ensureBranch(context.Background(), "proj", "feature", "")
		if err == nil || !strings.Contains(err.Error(), "create_branch_from") {
			t.Fatalf("want the create_branch_from hint, got: %v", err)
		}
	})

	t.Run("creates branch from ref", func(t *testing.T) {
		var createBody string
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
				http.Error(w, "no branch", http.StatusNotFound)
			case r.Method == http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/branches"):
				b, _ := io.ReadAll(r.Body)
				createBody = string(b)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"name":"feature","commit":{"id":"base"}}`))
			default:
				t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}
		})
		r := &filesResource{client: client}
		if err := r.ensureBranch(context.Background(), "proj", "feature", "main"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(createBody, `"branch":"feature"`) || !strings.Contains(createBody, `"ref":"main"`) {
			t.Errorf("create-branch payload must carry branch and ref, got: %s", createBody)
		}
	})

	t.Run("creation fails", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
				http.Error(w, "no branch", http.StatusNotFound)
			case r.Method == http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
			default:
				http.Error(w, "bad ref", http.StatusBadRequest)
			}
		})
		r := &filesResource{client: client}
		err := r.ensureBranch(context.Background(), "proj", "feature", "nope")
		if err == nil || !strings.Contains(err.Error(), `creating branch "feature" from create_branch_from ref "nope"`) {
			t.Fatalf("want a creating-branch error naming the attribute and ref, got: %v", err)
		}
	})
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
