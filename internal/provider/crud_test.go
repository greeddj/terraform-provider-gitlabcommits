// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// fileJSON renders a GetFile response body the way GitLab does (base64
// content), for handlers that answer the adopt probe's content fetch.
func fileJSON(filePath, blob, lcid string, content []byte) []byte {
	return []byte(fmt.Sprintf(`{"file_path":%q,"blob_id":%q,"content":%q,"encoding":"base64","last_commit_id":%q,"size":%d}`,
		filePath, blob, base64.StdEncoding.EncodeToString(content), lcid, len(content)))
}

// metaHeaders answers a metadata probe (HEAD) for an existing file.
func metaHeaders(w http.ResponseWriter, blob, lcid string, exec bool) {
	w.Header().Set("X-Gitlab-Blob-Id", blob)
	w.Header().Set("X-Gitlab-Last-Commit-Id", lcid)
	w.Header().Set("X-Gitlab-File-Path", "f.txt")
	w.Header().Set("X-Gitlab-Ref", "main")
	w.Header().Set("X-Gitlab-Size", "3")
	w.Header().Set("X-Gitlab-Execute-Filemode", fmt.Sprint(exec))
	w.WriteHeader(http.StatusOK)
}

func runDelete(t *testing.T, client *gitlab.Client, state filesResourceModel) *resource.DeleteResponse {
	t.Helper()
	return runDeleteOn(t, newTestResource(client), state)
}

func runDeleteOn(t *testing.T, res *filesResource, state filesResourceModel) *resource.DeleteResponse {
	t.Helper()
	req, resp := deleteRequest(t, res, state)
	res.Delete(t.Context(), req, resp)
	return resp
}

// deleteRequest is the Delete counterpart of updateRequest: the response
// starts as the prior state, as fwserver hands it to Delete.
func deleteRequest(t *testing.T, res *filesResource, state filesResourceModel) (resource.DeleteRequest, *resource.DeleteResponse) {
	t.Helper()
	ctx := t.Context()

	sresp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, sresp)
	sch := sresp.Schema

	st := tfsdk.State{Schema: sch}
	if d := st.Set(ctx, &state); d.HasError() {
		t.Fatalf("state.Set: %v", d)
	}
	return resource.DeleteRequest{State: st}, &resource.DeleteResponse{State: tfsdk.State{Schema: sch, Raw: st.Raw.Copy()}}
}

// repoFake is a small stateful GitLab: files maps every path on the branch
// to its last commit id. Like GitLab it answers a metadata probe and a tree
// listing from that map and rejects with HTTP 400, landing nothing, a commit
// that creates an existing path, or updates, deletes or chmods a missing one
// or with a stale last_commit_id; an accepted commit is applied to the map.
// branchStatus, commitStatus and probeStatus (per path) override the answers
// for GET /branches/, a commit and a probe. Every file holds the content
// "x", which a content fetch returns, unless content holds other bytes for
// its path. Every project_id spelling reaches the same branch; projects
// answers a project lookup with the numeric id for a spelling
// (projectStatus overrides it), and while it is nil a lookup is an
// unexpected call. beforeCommit, when set, runs as a commit arrives and
// before it is handled, without holding the fake's lock. Commits, probes
// (metadata requests) and project lookups are recorded, accepted or not.
type repoFake struct {
	files         map[string]string
	content       map[string][]byte
	probeStatus   map[string]int
	projects      map[string]int64
	beforeCommit  func()
	commits       [][]string
	probes        []string
	lookups       []string
	mu            sync.Mutex
	branchStatus  int
	commitStatus  int
	projectStatus int
}

func (f *repoFake) client(t *testing.T) *gitlab.Client {
	t.Helper()
	return newReadClient(t, f.handler(t))
}

// handler serves the fake, for a test that wraps it in a handler of its own.
func (f *repoFake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && f.beforeCommit != nil {
			f.beforeCommit()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		_, filePath, isFile := strings.Cut(r.URL.Path, "/repository/files/")
		project, isProject := strings.CutPrefix(r.URL.Path, "/api/v4/projects/")
		isProject = isProject && !strings.Contains(project, "/")
		switch {
		case r.Method == http.MethodHead && isFile:
			p := filePath
			f.probes = append(f.probes, p+"@"+r.URL.Query().Get("ref"))
			if status := f.probeStatus[p]; status != 0 {
				http.Error(w, "probe", status)
				return
			}
			lcid, ok := f.files[p]
			if !ok {
				http.Error(w, "404 File Not Found", http.StatusNotFound)
				return
			}
			metaHeaders(w, "blob-"+lcid, lcid, false)
		case r.Method == http.MethodGet && isFile:
			p := filePath
			lcid, ok := f.files[p]
			if !ok {
				http.Error(w, "404 File Not Found", http.StatusNotFound)
				return
			}
			content, ok := f.content[p]
			if !ok {
				content = []byte("x")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fileJSON(p, "blob-"+lcid, lcid, content))
		case isTreeRequest(r):
			var entries []gitlab.TreeNode
			for _, p := range slices.Sorted(maps.Keys(f.files)) {
				if strings.HasPrefix(p, r.URL.Query().Get("path")+"/") {
					entries = append(entries, gitlab.TreeNode{Path: p, Type: "blob"})
				}
			}
			if len(entries) == 0 {
				noDirectory(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(entries)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			if f.branchStatus != 0 {
				http.Error(w, "branch lookup", f.branchStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"head"}}`))
		case r.Method == http.MethodGet && isProject && f.projects != nil:
			f.lookups = append(f.lookups, project)
			id, ok := f.projects[project]
			switch {
			case f.projectStatus != 0:
				http.Error(w, "project lookup", f.projectStatus)
			case !ok:
				http.Error(w, "404 Project Not Found", http.StatusNotFound)
			default:
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":%d}`, id)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/commits"):
			var opts gitlab.CreateCommitOptions
			if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
				t.Errorf("decoding the commit body: %v", err)
				http.Error(w, "bad body", http.StatusInternalServerError)
				return
			}
			f.commits = append(f.commits, describeActions(opts.Actions))
			if f.commitStatus != 0 {
				http.Error(w, "commit", f.commitStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if msg := f.rejection(opts.Actions); msg != "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
				return
			}
			sha := fmt.Sprintf("sha%d", len(f.commits))
			for _, a := range opts.Actions {
				if *a.Action == gitlab.FileDelete {
					delete(f.files, *a.FilePath)
				} else {
					f.files[*a.FilePath] = sha
				}
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":%q}`, sha)
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}
}

// isTreeRequest reports whether r lists a repository tree, which Create and
// Update do for every path they create.
func isTreeRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repository/tree")
}

// noDirectory answers a tree listing as GitLab does for a path that is
// missing or holds a file.
func noDirectory(w http.ResponseWriter) {
	http.Error(w, `{"message":"404 invalid revision or path Not Found"}`, http.StatusNotFound)
}

// rejection is GitLab's message for a commit it refuses, or "".
func (f *repoFake) rejection(actions []*gitlab.CommitActionOptions) string {
	if f.branchStatus == http.StatusNotFound {
		return "You can only create or edit files when you are on a branch"
	}
	for _, a := range actions {
		lcid, ok := f.files[*a.FilePath]
		switch {
		case *a.Action == gitlab.FileCreate:
			if ok {
				return "A file with this name already exists"
			}
		case a.LastCommitID != nil && (!ok || *a.LastCommitID != lcid):
			return "The file has changed since you started editing it: " + *a.FilePath
		case !ok:
			return "A file with this name doesn't exist"
		}
	}
	return ""
}

// recorded returns the commits and the probes (sorted, since they fan out)
// sent so far, and the files left on the branch.
func (f *repoFake) recorded() (commits [][]string, probes []string, files map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	probes = slices.Clone(f.probes)
	slices.Sort(probes)
	return slices.Clone(f.commits), probes, maps.Clone(f.files)
}

// projectLookups returns the project lookups made so far, in order.
func (f *repoFake) projectLookups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lookups)
}

// describeActions renders actions as "<action>:<path>", plus "@<token>" when
// a last_commit_id was sent, so a test can pin a commit body exactly.
func describeActions(actions []*gitlab.CommitActionOptions) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		s := fmt.Sprintf("%s:%s", *a.Action, *a.FilePath)
		if a.LastCommitID != nil {
			s += "@" + *a.LastCommitID
		}
		out = append(out, s)
	}
	return out
}

// managedFiles returns a state managing paths, each with the stored
// last_commit_id given ("" stores none) and optimistic_lock set to lock.
func managedFiles(lock bool, tokens map[string]string) filesResourceModel {
	s := readState("blob")
	s.OptimisticLock = types.BoolValue(lock)
	s.Files = make(map[string]fileModel, len(tokens))
	for p, lcid := range tokens {
		s.Files[p] = fileModel{
			Content: types.StringValue("x"), ContentBase64: types.StringNull(),
			BlobID: types.StringValue("blob"), LastCommitID: stringOrNull(lcid), ExecuteFilemode: types.BoolValue(false),
		}
	}
	return s
}

// inFlight records how many requests overlap at most.
type inFlight struct {
	cur, peak atomic.Int32
}

func (f *inFlight) enter() {
	n := f.cur.Add(1)
	for {
		m := f.peak.Load()
		if n <= m || f.peak.CompareAndSwap(m, n) {
			return
		}
	}
}

func (f *inFlight) leave() { f.cur.Add(-1) }

func runUpdate(t *testing.T, client *gitlab.Client, plan, state filesResourceModel) *resource.UpdateResponse {
	t.Helper()
	return runUpdateOn(t, newTestResource(client), plan, state)
}

func runUpdateOn(t *testing.T, res *filesResource, plan, state filesResourceModel) *resource.UpdateResponse {
	t.Helper()
	req, resp := updateRequest(t, res, plan, state)
	res.Update(t.Context(), req, resp)
	checkUpdateResult(t, req, resp)
	return resp
}

// updateRequest builds the plan/state pair for res.Update the way fwserver
// does: the plan carries the unknowns plannedModel adds, and the response
// starts as the prior state. Kept apart from runUpdateOn so concurrency
// tests build requests on the test goroutine (t.Fatalf must not run
// anywhere else) and only call Update from workers.
func updateRequest(t *testing.T, res *filesResource, plan, state filesResourceModel) (resource.UpdateRequest, *resource.UpdateResponse) {
	t.Helper()
	ctx := t.Context()

	sresp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, sresp)
	sch := sresp.Schema

	pl := tfsdk.Plan{Schema: sch}
	planned := plannedModel(plan, false)
	if d := pl.Set(ctx, &planned); d.HasError() {
		t.Fatalf("plan.Set: %v", d)
	}
	st := tfsdk.State{Schema: sch}
	if d := st.Set(ctx, &state); d.HasError() {
		t.Fatalf("state.Set: %v", d)
	}
	return resource.UpdateRequest{Plan: pl, State: st}, &resource.UpdateResponse{State: tfsdk.State{Schema: sch, Raw: st.Raw.Copy()}}
}

func runCreate(t *testing.T, client *gitlab.Client, plan filesResourceModel) *resource.CreateResponse {
	t.Helper()
	return runCreateOn(t, newTestResource(client), plan)
}

func runCreateOn(t *testing.T, res *filesResource, plan filesResourceModel) *resource.CreateResponse {
	t.Helper()
	req, resp := createRequest(t, res, plan)
	res.Create(t.Context(), req, resp)
	checkCreateResult(t, resp)
	return resp
}

// createRequest is the Create counterpart of updateRequest; the response
// starts as the typed null state fwserver hands Create.
func createRequest(t *testing.T, res *filesResource, plan filesResourceModel) (resource.CreateRequest, *resource.CreateResponse) {
	t.Helper()
	ctx := t.Context()

	sresp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, sresp)
	sch := sresp.Schema

	pl := tfsdk.Plan{Schema: sch}
	planned := plannedModel(plan, true)
	if d := pl.Set(ctx, &planned); d.HasError() {
		t.Fatalf("plan.Set: %v", d)
	}
	empty := tftypes.NewValue(sch.Type().TerraformType(ctx), nil)
	return resource.CreateRequest{Plan: pl}, &resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: empty}}
}

// plannedModel returns m as the framework plans it: every Computed attribute
// with no default and no configured value is unknown (known after apply),
// except id on Update, which UseStateForUnknown keeps at its prior value.
// The files map is copied, so a state model sharing it never sees the
// unknowns.
func plannedModel(m filesResourceModel, create bool) filesResourceModel {
	m.Files = maps.Clone(m.Files)
	for p, f := range m.Files {
		f.BlobID = types.StringUnknown()
		f.LastCommitID = types.StringUnknown()
		m.Files[p] = f
	}
	m.CommitSHA = types.StringUnknown()
	if create {
		m.ID = types.StringUnknown()
	}
	return m
}

// checkCreateResult pins what a Create may hand back. Success is a fully
// known state: Terraform rejects unknowns after apply, when the commit has
// already landed. Failure is no state at all: a Create that returns state
// next to an error is tainted and replaced, which pushes a delete commit.
// It only calls t.Error, so workers may use it.
func checkCreateResult(t *testing.T, resp *resource.CreateResponse) {
	t.Helper()
	switch {
	case resp.Diagnostics.HasError():
		if !resp.State.Raw.IsNull() {
			t.Errorf("a failed Create must return no state, got: %s", resp.State.Raw)
		}
	case resp.State.Raw.IsNull():
		t.Error("a successful Create must return state")
	case !resp.State.Raw.IsFullyKnown():
		t.Errorf("Create returned state with unknown values: %s", resp.State.Raw)
	}
}

// checkUpdateResult is the Update counterpart: success is a fully known
// state, and failure leaves the prior state exactly as it was, since
// anything else would record content that never landed.
func checkUpdateResult(t *testing.T, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	t.Helper()
	switch {
	case resp.Diagnostics.HasError():
		if !resp.State.Raw.Equal(req.State.Raw) {
			t.Errorf("a failed Update must leave the prior state in place, got: %s", resp.State.Raw)
		}
	case resp.State.Raw.IsNull():
		t.Error("a successful Update must return state")
	case !resp.State.Raw.IsFullyKnown():
		t.Errorf("Update returned state with unknown values: %s", resp.State.Raw)
	}
}

// TestCreate_NullCommitBodyErrors pins the nil-commit guard in Create: a 2xx
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

// TestUpdate_NullCommitBodyErrors pins the nil-commit guard in Update.
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

// TestCommitConflict_LeavesStateUntouched: when GitLab rejects the commit
// under optimistic_lock, Create returns no state (state next to an error is
// tainted and replaced with a delete commit) and Update keeps the prior
// state (the planned content never landed).
func TestCommitConflict_LeavesStateUntouched(t *testing.T) {
	conflict := func(t *testing.T, posts *atomic.Int32) *gitlab.Client {
		return newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
			case r.Method == http.MethodHead:
				metaHeaders(w, "remoteblob", "remote-lcid", false)
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/files/"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(fileJSON("f.txt", "remoteblob", "remote-lcid", []byte("remote")))
			case r.Method == http.MethodPost:
				posts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"You are attempting to update a file that has changed since you started editing it."}`))
			default:
				t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}
		})
	}
	wantConflict := func(t *testing.T, diags diag.Diagnostics, posts *atomic.Int32) {
		t.Helper()
		if !diags.HasError() || !strings.Contains(diags.Errors()[0].Summary(), "Concurrent modification detected") {
			t.Fatalf("expected the concurrent-modification diagnostic, got %v", diags)
		}
		if got := posts.Load(); got != 1 {
			t.Errorf("commit POSTs = %d, want 1", got)
		}
	}

	t.Run("create", func(t *testing.T) {
		var posts atomic.Int32
		res := newTestResource(conflict(t, &posts))
		req, resp := createRequest(t, res, readState("ignored"))
		res.Create(t.Context(), req, resp)
		wantConflict(t, resp.Diagnostics, &posts)
		if !resp.State.Raw.IsNull() {
			t.Errorf("a rejected Create must return no state, got: %s", resp.State.Raw)
		}
	})

	t.Run("update", func(t *testing.T) {
		var posts atomic.Int32
		res := newTestResource(conflict(t, &posts))
		plan, state := changedPlan()
		req, resp := updateRequest(t, res, plan, state)
		res.Update(t.Context(), req, resp)
		wantConflict(t, resp.Diagnostics, &posts)
		if !resp.State.Raw.Equal(req.State.Raw) {
			t.Errorf("a rejected Update must keep the prior state, got: %s", resp.State.Raw)
		}
	})
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

// TestDelete_Outcomes pins how destroy reaches its one commit. A delete that
// carries a last_commit_id goes out unprobed, since GitLab checks the token
// itself; only a rejection (a 400, or the 404 of a project that is gone)
// makes Delete probe and retry once without the files already gone, and any
// other failure is reported as it is. A delete without a token is probed
// first, because GitLab would remove whatever sits at the path. A 404 counts
// only once the branch lookup succeeds, and a destroy that finds nothing to
// delete warns.
func TestDelete_Outcomes(t *testing.T) {
	cases := []struct {
		repo         map[string]string // path -> last commit id on the branch
		tokens       map[string]string // managed path -> stored last_commit_id
		probeStatus  map[string]int
		wantRepo     map[string]string
		name         string
		wantError    string
		wantWarning  string
		wantCommits  [][]string
		wantProbes   []string
		branchStatus int
		commitStatus int
		lock         bool
	}{
		{
			name: "nothing managed makes no request",
			lock: true,
		},
		{
			name:        "locked deletes commit without probing",
			lock:        true,
			repo:        map[string]string{"a.txt": "la", "b.txt": "lb"},
			tokens:      map[string]string{"a.txt": "la", "b.txt": "lb"},
			wantCommits: [][]string{{"delete:a.txt@la", "delete:b.txt@lb"}},
		},
		{
			name:        "rejected commit is retried without the file already gone",
			lock:        true,
			repo:        map[string]string{"keep.txt": "lk"},
			tokens:      map[string]string{"gone.txt": "lg", "keep.txt": "lk"},
			wantCommits: [][]string{{"delete:gone.txt@lg", "delete:keep.txt@lk"}, {"delete:keep.txt@lk"}},
			wantProbes:  []string{"gone.txt@main", "keep.txt@main"},
		},
		{
			name:        "rejection with every file present is reported as it is",
			lock:        true,
			repo:        map[string]string{"keep.txt": "external"},
			tokens:      map[string]string{"keep.txt": "lk"},
			wantCommits: [][]string{{"delete:keep.txt@lk"}},
			wantProbes:  []string{"keep.txt@main"},
			wantError:   "Concurrent modification detected",
			wantRepo:    map[string]string{"keep.txt": "external"},
		},
		{
			name:        "second rejection fails with its own diagnostic",
			lock:        true,
			repo:        map[string]string{"keep.txt": "external"},
			tokens:      map[string]string{"gone.txt": "lg", "keep.txt": "lk"},
			wantCommits: [][]string{{"delete:gone.txt@lg", "delete:keep.txt@lk"}, {"delete:keep.txt@lk"}},
			wantProbes:  []string{"gone.txt@main", "keep.txt@main"},
			wantError:   "editing it: keep.txt",
			wantRepo:    map[string]string{"keep.txt": "external"},
		},
		{
			name:        "everything already gone warns without a second commit",
			lock:        true,
			tokens:      map[string]string{"a.txt": "la", "b.txt": "lb"},
			wantCommits: [][]string{{"delete:a.txt@la", "delete:b.txt@lb"}},
			wantProbes:  []string{"a.txt@main", "b.txt@main"},
			wantWarning: "removed out of band",
		},
		{
			name:         "a vanished branch warns that the files may remain",
			lock:         true,
			tokens:       map[string]string{"a.txt": "la"},
			branchStatus: http.StatusNotFound,
			wantCommits:  [][]string{{"delete:a.txt@la"}},
			wantProbes:   []string{"a.txt@main"},
			wantWarning:  "may still exist",
		},
		{
			name:         "a 404 is not trusted when the branch lookup fails",
			lock:         true,
			repo:         map[string]string{"keep.txt": "lk"},
			tokens:       map[string]string{"gone.txt": "lg", "keep.txt": "lk"},
			branchStatus: http.StatusInternalServerError,
			wantCommits:  [][]string{{"delete:gone.txt@lg", "delete:keep.txt@lk"}},
			wantProbes:   []string{"gone.txt@main", "keep.txt@main"},
			wantError:    "Gitaly times out",
			wantRepo:     map[string]string{"keep.txt": "lk"},
		},
		{
			name:        "a failing probe after a rejection fails the destroy",
			lock:        true,
			repo:        map[string]string{"keep.txt": "lk"},
			tokens:      map[string]string{"gone.txt": "lg", "keep.txt": "lk"},
			probeStatus: map[string]int{"keep.txt": http.StatusForbidden},
			wantCommits: [][]string{{"delete:gone.txt@lg", "delete:keep.txt@lk"}},
			wantProbes:  []string{"gone.txt@main", "keep.txt@main"},
			wantError:   "HTTP 403",
			wantRepo:    map[string]string{"keep.txt": "lk"},
		},
		{
			name:         "a refused commit is reported without probing",
			lock:         true,
			repo:         map[string]string{"a.txt": "la"},
			tokens:       map[string]string{"a.txt": "la"},
			commitStatus: http.StatusForbidden,
			wantCommits:  [][]string{{"delete:a.txt@la"}},
			wantError:    "HTTP 403",
			wantRepo:     map[string]string{"a.txt": "la"},
		},
		{
			name:         "a project that is gone warns that the files may remain",
			lock:         true,
			tokens:       map[string]string{"a.txt": "la"},
			branchStatus: http.StatusNotFound,
			commitStatus: http.StatusNotFound,
			wantCommits:  [][]string{{"delete:a.txt@la"}},
			wantProbes:   []string{"a.txt@main"},
			wantWarning:  "may still exist",
		},
		{
			name:         "a 404 with every file present is reported as it is",
			lock:         true,
			repo:         map[string]string{"a.txt": "la"},
			tokens:       map[string]string{"a.txt": "la"},
			commitStatus: http.StatusNotFound,
			wantCommits:  [][]string{{"delete:a.txt@la"}},
			wantProbes:   []string{"a.txt@main"},
			wantError:    "HTTP 404",
			wantRepo:     map[string]string{"a.txt": "la"},
		},
		{
			name:        "without the lock every path is probed and no token is sent",
			repo:        map[string]string{"keep.txt": "lk"},
			tokens:      map[string]string{"gone.txt": "lg", "keep.txt": "lk"},
			wantCommits: [][]string{{"delete:keep.txt"}},
			wantProbes:  []string{"gone.txt@main", "keep.txt@main"},
		},
		{
			name:        "a path with no stored token is probed under the lock",
			lock:        true,
			repo:        map[string]string{"bare.txt": "lb", "keep.txt": "lk"},
			tokens:      map[string]string{"bare.txt": "", "gone.txt": "", "keep.txt": "lk"},
			wantCommits: [][]string{{"delete:bare.txt", "delete:keep.txt@lk"}},
			wantProbes:  []string{"bare.txt@main", "gone.txt@main"},
		},
		{
			name:        "without the lock nothing present warns and commits nothing",
			tokens:      map[string]string{"a.txt": "la"},
			wantProbes:  []string{"a.txt@main"},
			wantWarning: "removed out of band",
		},
		{
			name:         "without the lock a hidden branch warns that the files may remain",
			tokens:       map[string]string{"a.txt": "la"},
			branchStatus: http.StatusNotFound,
			wantProbes:   []string{"a.txt@main"},
			wantWarning:  "may still exist",
		},
		{
			name:         "without the lock a 404 is not trusted when the branch lookup fails",
			repo:         map[string]string{"keep.txt": "lk"},
			tokens:       map[string]string{"gone.txt": "lg", "keep.txt": "lk"},
			branchStatus: http.StatusInternalServerError,
			wantProbes:   []string{"gone.txt@main", "keep.txt@main"},
			wantError:    "Gitaly times out",
			wantRepo:     map[string]string{"keep.txt": "lk"},
		},
		{
			name:        "without the lock a failing probe fails the destroy",
			repo:        map[string]string{"keep.txt": "lk"},
			tokens:      map[string]string{"gone.txt": "lg", "keep.txt": "lk"},
			probeStatus: map[string]int{"keep.txt": http.StatusForbidden},
			wantProbes:  []string{"gone.txt@main", "keep.txt@main"},
			wantError:   "HTTP 403",
			wantRepo:    map[string]string{"keep.txt": "lk"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &repoFake{files: maps.Clone(c.repo), probeStatus: c.probeStatus, branchStatus: c.branchStatus, commitStatus: c.commitStatus}
			if fake.files == nil {
				fake.files = map[string]string{}
			}
			resp := runDelete(t, fake.client(t), managedFiles(c.lock, c.tokens))

			commits, probes, repo := fake.recorded()
			if !slices.EqualFunc(commits, c.wantCommits, slices.Equal[[]string]) {
				t.Errorf("commits = %q, want %q", commits, c.wantCommits)
			}
			if !slices.Equal(probes, c.wantProbes) {
				t.Errorf("probes = %q, want %q", probes, c.wantProbes)
			}
			if !maps.Equal(repo, c.wantRepo) {
				t.Errorf("files left on the branch = %v, want %v", repo, c.wantRepo)
			}
			wantDiag(t, "error", resp.Diagnostics.Errors(), c.wantError)
			wantDiag(t, "warning", resp.Diagnostics.Warnings(), c.wantWarning)
		})
	}
}

// wantDiag asserts diags holds exactly one entry mentioning want in its
// summary or detail, or none at all when want is empty.
func wantDiag(t *testing.T, kind string, diags diag.Diagnostics, want string) {
	t.Helper()
	switch {
	case want == "" && len(diags) > 0:
		t.Errorf("unexpected %s: %v", kind, diags)
	case want != "" && (len(diags) != 1 || !strings.Contains(diags[0].Summary()+" "+diags[0].Detail(), want)):
		t.Errorf("%s diagnostics = %v, want exactly one mentioning %q", kind, diags, want)
	}
}

// TestDelete_ConcurrentSameBranchCommitsAreSerialised: destroying a for_each
// set that shares one branch runs every Delete at once, and their commits
// must never be in flight together.
func TestDelete_ConcurrentSameBranchCommitsAreSerialised(t *testing.T) {
	var posts atomic.Int32
	var overlap inFlight
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected call %s %s: a locked delete needs no probe", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		posts.Add(1)
		overlap.enter()
		// Long enough that unserialised goroutines provably overlap.
		time.Sleep(20 * time.Millisecond)
		overlap.leave()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"delsha"}`))
	})
	locks := newBranchLocks()

	var wg sync.WaitGroup
	for range 4 {
		res := &filesResource{client: client, locks: locks}
		req, resp := deleteRequest(t, res, readState("blob"))
		wg.Go(func() {
			res.Delete(t.Context(), req, resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("unexpected error: %v", resp.Diagnostics.Errors())
			}
		})
	}
	wg.Wait()
	if got := overlap.peak.Load(); got != 1 {
		t.Fatalf("destroy commits to one branch overlapped: max in flight = %d, want 1", got)
	}
	if got := posts.Load(); got != 4 {
		t.Errorf("commits = %d, want one per instance", got)
	}
}

// TestUpdate_UnguardedDeleteAndChmodAreProbed: a delete or chmod without a
// last_commit_id has no guard at GitLab, so its path is probed first; a
// delete of a path that no longer holds a file is dropped and a chmod of one
// fails. With the lock on and every token present nothing is probed before
// the commit, and a path the adopt probe has just found is not probed again.
// Every plan drops rm.txt and sets run.sh's exec bit to chmod; adopt adds
// new.sh, executable, with the content the branch already holds.
func TestUpdate_UnguardedDeleteAndChmodAreProbed(t *testing.T) {
	cases := []struct {
		repo               map[string]string
		tokens             map[string]string
		name               string
		wantError          string
		wantCommits        [][]string
		wantProbes         []string
		branchStatus       int
		lock, chmod, adopt bool
	}{
		{
			name:        "locked actions are not probed",
			lock:        true,
			chmod:       true,
			repo:        map[string]string{"rm.txt": "lr", "run.sh": "ls"},
			wantCommits: [][]string{{"delete:rm.txt@lr", "chmod:run.sh@ls"}},
			wantProbes:  []string{"run.sh@sha1"},
		},
		{
			name:        "without the lock both paths are probed first",
			chmod:       true,
			repo:        map[string]string{"rm.txt": "lr", "run.sh": "ls"},
			wantCommits: [][]string{{"delete:rm.txt", "chmod:run.sh"}},
			wantProbes:  []string{"rm.txt@main", "run.sh@main", "run.sh@sha1"},
		},
		{
			name:        "a delete of a path already gone is dropped",
			chmod:       true,
			repo:        map[string]string{"run.sh": "ls"},
			wantCommits: [][]string{{"chmod:run.sh"}},
			wantProbes:  []string{"rm.txt@main", "run.sh@main", "run.sh@sha1"},
		},
		{
			name:       "dropping the only action makes no commit",
			repo:       map[string]string{"run.sh": "ls"},
			wantProbes: []string{"rm.txt@main"},
		},
		{
			name:       "a chmod of a path that no longer holds a file fails",
			chmod:      true,
			repo:       map[string]string{"rm.txt": "lr"},
			wantProbes: []string{"rm.txt@main", "run.sh@main"},
			wantError:  `"run.sh"`,
		},
		{
			name:        "a missing token is probed under the lock",
			lock:        true,
			chmod:       true,
			repo:        map[string]string{"run.sh": "ls"},
			tokens:      map[string]string{"rm.txt": "", "run.sh": "ls"},
			wantCommits: [][]string{{"chmod:run.sh@ls"}},
			wantProbes:  []string{"rm.txt@main", "run.sh@sha1"},
		},
		{
			name:         "a 404 is not trusted when the branch lookup fails",
			chmod:        true,
			repo:         map[string]string{"run.sh": "ls"},
			branchStatus: http.StatusInternalServerError,
			wantProbes:   []string{"rm.txt@main", "run.sh@main"},
			wantError:    "Gitaly times out",
		},
		{
			name:        "an adopted chmod is not probed again",
			adopt:       true,
			repo:        map[string]string{"new.sh": "ln", "rm.txt": "lr", "run.sh": "ls"},
			wantCommits: [][]string{{"delete:rm.txt", "chmod:new.sh"}},
			wantProbes:  []string{"new.sh@main", "new.sh@sha1", "rm.txt@main"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &repoFake{files: maps.Clone(c.repo), branchStatus: c.branchStatus}
			tokens := c.tokens
			if tokens == nil {
				tokens = map[string]string{"rm.txt": "lr", "run.sh": "ls"}
			}
			state := managedFiles(c.lock, tokens)
			plan := managedFiles(c.lock, tokens)
			delete(plan.Files, "rm.txt")
			run := plan.Files["run.sh"]
			run.ExecuteFilemode = types.BoolValue(c.chmod)
			plan.Files["run.sh"] = run
			if c.adopt {
				run.ExecuteFilemode = types.BoolValue(true)
				plan.Files["new.sh"] = run
			}

			resp := runUpdate(t, fake.client(t), plan, state)

			commits, probes, _ := fake.recorded()
			if !slices.EqualFunc(commits, c.wantCommits, slices.Equal[[]string]) {
				t.Errorf("commits = %q, want %q", commits, c.wantCommits)
			}
			if !slices.Equal(probes, c.wantProbes) {
				t.Errorf("probes = %q, want %q", probes, c.wantProbes)
			}
			wantDiag(t, "error", resp.Diagnostics.Errors(), c.wantError)
			if c.wantError != "" || len(c.wantCommits) > 0 {
				return
			}
			var out filesResourceModel
			if d := resp.State.Get(t.Context(), &out); d.HasError() {
				t.Fatalf("state.Get: %v", d)
			}
			if _, kept := out.Files["rm.txt"]; kept || out.CommitSHA.ValueString() != "sha" {
				t.Errorf("a dropped delete must leave rm.txt out of state and commit_sha as it was, got files=%v sha=%q",
					slices.Sorted(maps.Keys(out.Files)), out.CommitSHA.ValueString())
			}
		})
	}
}

// TestCommitLocked_CancelledWaitSendsNothing: a commit still waiting for the
// branch lock when ctx ends is never sent, and says so.
func TestCommitLocked_CancelledWaitSendsNothing(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call %s %s while the branch lock is held", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	res := newTestResource(client)
	release, err := res.locks.acquire(t.Context(), "proj", "main")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	plan, state := changedPlan()
	req, resp := updateRequest(t, res, plan, state)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	res.Update(ctx, req, resp)
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "Cancelled while waiting for the branch lock" {
		t.Fatalf("expected the lock-wait diagnostic, got %v", resp.Diagnostics)
	}
	checkUpdateResult(t, req, resp)
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

// TestCreate_EmptyRepository: on a project with no commits every branch
// lookup 404s and no ref exists to branch from. Without create_branch_from,
// Create sends one commit naming the branch alone, which GitLab makes the
// root commit together with the branch; nothing can exist yet, so nothing is
// probed before it. With create_branch_from set the ref cannot exist, and
// Create says to remove it rather than ignoring it.
func TestCreate_EmptyRepository(t *testing.T) {
	for _, withFrom := range []bool{false, true} {
		t.Run(fmt.Sprintf("create_branch_from set %v", withFrom), func(t *testing.T) {
			var bodies []string
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
					http.Error(w, "no branch", http.StatusNotFound)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
					projectJSON(w, true)
				case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "rootsha":
					stampHeaders(w, "rootsha")
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/commits"):
					b, _ := io.ReadAll(r.Body)
					bodies = append(bodies, string(b))
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":"rootsha"}`))
				default:
					t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})

			plan := readState("blob")
			if withFrom {
				plan.CreateBranchFrom = types.StringValue("main")
			}
			resp := runCreate(t, client, plan)
			if withFrom {
				if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "remove create_branch_from") {
					t.Fatalf("want the diagnostic to say to remove create_branch_from, got: %v", resp.Diagnostics)
				}
				if len(bodies) != 0 {
					t.Errorf("commits = %d, want none", len(bodies))
				}
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			if len(bodies) != 1 {
				t.Fatalf("commits = %d, want 1", len(bodies))
			}
			body := bodies[0]
			if !strings.Contains(body, `"branch":"main"`) || strings.Contains(body, "start_") || !strings.Contains(body, `"action":"create"`) {
				t.Errorf("the first commit must name the branch alone and create the file, body: %s", body)
			}
			var out filesResourceModel
			if d := resp.State.Get(t.Context(), &out); d.HasError() {
				t.Fatalf("state.Get: %v", d)
			}
			if out.CommitSHA.ValueString() != "rootsha" || out.Files["f.txt"].LastCommitID.ValueString() != "rootsha" {
				t.Errorf("state must record the root commit, got commit_sha=%q", out.CommitSHA.ValueString())
			}
		})
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
		case isTreeRequest(r):
			noDirectory(w)
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
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/files/"):
			// Adopt content compare: remote differs, so the update stands.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fileJSON("f.txt", "remoteblob", "adopt-lcid", []byte("remote")))
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
		_, err := r.missingBranchPreflight(t.Context(), "proj", "feature", "main")
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
		_, err := r.missingBranchPreflight(t.Context(), "proj", "feature", "")
		if err == nil || !strings.Contains(err.Error(), "create_branch_from") {
			t.Fatalf("want the create_branch_from hint, got: %v", err)
		}
	})

}

// sourceHead is the head commit of create_branch_from = "main" in the fakes
// of a missing target branch.
const sourceHead = "5eed00000000000000000000000000000000a1a1"

// branchJSON answers a branch lookup with the given head commit.
func branchJSON(w http.ResponseWriter, name, head string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"name":%q,"commit":{"id":%q}}`, name, head)
}

// projectJSON answers the project lookup of the missing-branch preflight.
func projectJSON(w http.ResponseWriter, empty bool) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":1,"empty_repo":%t}`, empty)
}

// TestCreate_FirstCommitRejectionNamesRef: when GitLab rejects the first
// commit that was to materialise the branch, the diagnostic names the branch,
// the create_branch_from ref and the commit it resolved to, since that
// pairing is the usual culprit.
func TestCreate_FirstCommitRejectionNamesRef(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
			branchJSON(w, "main", sourceHead)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			http.Error(w, "no branch yet", http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
			projectJSON(w, false)
		case r.Method == http.MethodHead:
			http.Error(w, "absent", http.StatusNotFound)
		case isTreeRequest(r):
			noDirectory(w)
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Push rule rejected the branch"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	plan := readState("ignored")
	plan.Branch = types.StringValue("feature")
	plan.CreateBranchFrom = types.StringValue("main")

	resp := runCreate(t, client, plan)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected the rejected first commit to fail Create")
	}
	summary := resp.Diagnostics.Errors()[0].Summary()
	if want := `creating branch "feature" from create_branch_from ref "main" at commit ` + sourceHead; !strings.Contains(summary, want) {
		t.Errorf("summary must name the branch, ref and commit, got: %q", summary)
	}
}

// TestCreate_AdoptsFromCreateBranchFromRef is the regression test for
// adoption across branch materialisation: when the branch does not exist yet
// and create_branch_from points at a branch that already contains a managed
// path, the adopt probe must resolve against that branch's head commit - the
// new branch inherits the file, so a plain create would die with "already
// exists". The branch itself is materialised by start_sha on the commit,
// never by a separate branch-creation call.
func TestCreate_AdoptsFromCreateBranchFromRef(t *testing.T) {
	var commitBody string
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
			branchJSON(w, "main", sourceHead)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			http.Error(w, "no branch yet", http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
			projectJSON(w, false)
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == sourceHead:
			// The managed path already exists on the source branch.
			metaHeaders(w, "srcblob", "src-lcid", false)
		case r.Method == http.MethodGet && r.URL.Query().Get("ref") == sourceHead && strings.Contains(r.URL.Path, "/repository/files/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fileJSON("f.txt", "srcblob", "src-lcid", []byte("remote")))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "absha":
			stampHeaders(w, "absha")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/branches"):
			t.Error("the branch must be created by start_sha on the commit, not by a separate call")
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
	plan.CreateBranchFrom = types.StringValue("main")

	resp := runCreate(t, client, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if !strings.Contains(commitBody, `"start_sha":"`+sourceHead+`"`) || strings.Contains(commitBody, "start_branch") {
		t.Errorf("the first commit must materialise the branch via start_sha at the resolved head, body: %s", commitBody)
	}
	if !strings.Contains(commitBody, `"action":"update"`) {
		t.Errorf("inherited path must be adopted as an update, body: %s", commitBody)
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
		case isTreeRequest(r):
			noDirectory(w)
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
	var overlap inFlight
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			overlap.enter()
			// Long enough that unserialised goroutines provably overlap.
			time.Sleep(20 * time.Millisecond)
			overlap.leave()
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
			checkUpdateResult(t, req, resp)
		})
	}
	wg.Wait()
	if got := overlap.peak.Load(); got != 1 {
		t.Fatalf("commits to one branch overlapped: max in flight = %d, want 1", got)
	}
}

// TestCreate_ConcurrentBranchMaterialisationIsSerialised: several instances
// creating on the same missing branch must not all try to materialise it.
// Create holds the branch lock from the existence check through the commit,
// so the first instance creates the branch with start_sha and the others
// then see it, probe it again and commit plainly.
func TestCreate_ConcurrentBranchMaterialisationIsSerialised(t *testing.T) {
	var branchCreated atomic.Bool
	var startSHACommits, commits, branchProbes atomic.Int32
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
			branchJSON(w, "main", sourceHead)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			if !branchCreated.Load() {
				http.Error(w, "no branch yet", http.StatusNotFound)
				return
			}
			branchJSON(w, "feature", "sha")
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
			projectJSON(w, false)
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "sha":
			stampHeaders(w, "sha")
		case r.Method == http.MethodHead:
			if r.URL.Query().Get("ref") == "feature" {
				branchProbes.Add(1)
			}
			http.Error(w, "absent", http.StatusNotFound)
		case isTreeRequest(r):
			noDirectory(w)
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			commits.Add(1)
			if strings.Contains(string(b), `"start_sha"`) {
				if branchCreated.Swap(true) {
					t.Error("start_sha sent for a branch that already exists")
				}
				startSHACommits.Add(1)
			} else if !branchCreated.Load() {
				t.Error("plain commit attempted before the branch was materialised")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sha"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	locks := newBranchLocks()

	var wg sync.WaitGroup
	for range 4 {
		res := &filesResource{client: client, locks: locks}
		plan := readState("ignored")
		plan.Branch = types.StringValue("feature")
		plan.CreateBranchFrom = types.StringValue("main")
		req, resp := createRequest(t, res, plan)
		wg.Go(func() {
			res.Create(t.Context(), req, resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			checkCreateResult(t, resp)
		})
	}
	wg.Wait()
	if got := startSHACommits.Load(); got != 1 {
		t.Errorf("start_sha commits = %d, want exactly 1", got)
	}
	if got := commits.Load(); got != 4 {
		t.Errorf("commits = %d, want one per instance", got)
	}
	if got := branchProbes.Load(); got != 3 {
		t.Errorf("probes of the branch = %d, want one per instance that found it created", got)
	}
}

// TestCreate_ConcurrentOnExistingBranchCommitsAreSerialised: the first apply
// of a for_each set onto an existing branch runs every Create at once, and
// their commits must never be in flight together.
func TestCreate_ConcurrentOnExistingBranchCommitsAreSerialised(t *testing.T) {
	var posts atomic.Int32
	var overlap inFlight
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "sha":
			stampHeaders(w, "sha")
		case r.Method == http.MethodHead:
			http.Error(w, "absent", http.StatusNotFound)
		case isTreeRequest(r):
			noDirectory(w)
		case r.Method == http.MethodPost:
			posts.Add(1)
			overlap.enter()
			time.Sleep(20 * time.Millisecond)
			overlap.leave()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sha"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	locks := newBranchLocks()

	var wg sync.WaitGroup
	for range 4 {
		res := &filesResource{client: client, locks: locks}
		req, resp := createRequest(t, res, readState("ignored"))
		wg.Go(func() {
			res.Create(t.Context(), req, resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			checkCreateResult(t, resp)
		})
	}
	wg.Wait()
	if got := overlap.peak.Load(); got != 1 {
		t.Fatalf("commits to one branch overlapped: max in flight = %d, want 1", got)
	}
	if got := posts.Load(); got != 4 {
		t.Errorf("commits = %d, want one per instance", got)
	}
}

// TestCreate_ErrorInsideLockReleasesIt: Create returns from inside its
// critical section when the branch re-check or the bare branch creation
// fails, and the lock must be free afterwards, or every sibling on that
// branch would wait until the apply is cancelled. f.txt exists at the
// source, so no directory check adds a branch lookup before the lock.
func TestCreate_ErrorInsideLockReleasesIt(t *testing.T) {
	for _, identical := range []bool{false, true} {
		name := "branch re-check fails"
		remote := "remote"
		if identical {
			// Adoption leaves nothing to commit, so the branch is created bare.
			name, remote = "bare branch creation fails", "old"
		}
		t.Run(name, func(t *testing.T) {
			var branchGets atomic.Int32
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
					branchJSON(w, "main", sourceHead)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
					if branchGets.Add(1) > 1 && !identical {
						http.Error(w, "unavailable", http.StatusInternalServerError)
						return
					}
					http.Error(w, "no branch yet", http.StatusNotFound)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
				case r.Method == http.MethodHead:
					metaHeaders(w, "remoteblob", "remote-lcid", false)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/files/"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(fileJSON("f.txt", "remoteblob", "remote-lcid", []byte(remote)))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/branches"):
					http.Error(w, "push rule", http.StatusForbidden)
				default:
					t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})
			res := newTestResource(client)
			plan := readState("ignored")
			plan.Branch = types.StringValue("feature")
			plan.CreateBranchFrom = types.StringValue("main")

			if resp := runCreateOn(t, res, plan); !resp.Diagnostics.HasError() {
				t.Fatal("expected Create to fail")
			}
			if got := branchGets.Load(); got < 2 {
				t.Fatalf("branch lookups = %d, want the failure under the lock", got)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			release, err := res.locks.acquire(ctx, "proj", "feature")
			if err != nil {
				t.Fatalf("the branch lock is still held after the failed Create: %v", err)
			}
			release()
		})
	}
}

// TestCreate_CommitSHASourceUsesStartSHA: a create_branch_from that is a
// full commit SHA is used as it is, with no branch lookup, and travels as
// start_sha. Gitaly takes only lowercase hex there, so an uppercase SHA is
// lowercased, for the probes as well.
func TestCreate_CommitSHASourceUsesStartSHA(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, configured := range []string{sha, strings.ToUpper(sha)} {
		t.Run(configured, func(t *testing.T) {
			var commitBody string
			var refs []string
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/feature"):
					http.Error(w, "no branch yet", http.StatusNotFound)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
					projectJSON(w, false)
				case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "newsha":
					stampHeaders(w, "newsha")
				case r.Method == http.MethodHead:
					refs = append(refs, r.URL.Query().Get("ref"))
					http.Error(w, "absent", http.StatusNotFound)
				case isTreeRequest(r):
					refs = append(refs, r.URL.Query().Get("ref"))
					noDirectory(w)
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

			plan := readState("ignored")
			plan.Branch = types.StringValue("feature")
			plan.CreateBranchFrom = types.StringValue(configured)
			if resp := runCreate(t, client, plan); resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			if !strings.Contains(commitBody, `"start_sha":"`+sha+`"`) || strings.Contains(commitBody, "start_branch") {
				t.Errorf("a SHA source must be sent as a lowercase start_sha only, body: %s", commitBody)
			}
			if want := []string{sha, sha}; !slices.Equal(refs, want) {
				t.Errorf("probe refs = %q, want %q", refs, want)
			}
		})
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
		case isTreeRequest(r):
			noDirectory(w)
		case r.Method == http.MethodPost:
			posts.Add(1)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
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

// adoptServer fakes a branch whose f.txt already holds remoteContent: the
// branch exists, the adopt probe finds the file, and the content fetch
// returns remoteContent. Commit POSTs are counted and answered with 201.
func adoptServer(t *testing.T, remoteContent string, posts *atomic.Int32, commitBody *string) *gitlab.Client {
	t.Helper()
	return newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "main":
			metaHeaders(w, "remoteblob", "remote-lcid", false)
		case r.Method == http.MethodHead:
			stampHeaders(w, "newsha")
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/files/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fileJSON("f.txt", "remoteblob", "remote-lcid", []byte(remoteContent)))
		case r.Method == http.MethodPost:
			posts.Add(1)
			b, _ := io.ReadAll(r.Body)
			*commitBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"newsha"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
}

// TestCreate_AdoptIdenticalContentMakesNoCommit: adopting a file whose remote
// bytes already equal the plan must not produce a commit; state is stamped
// from the probe and commit_sha stays null.
func TestCreate_AdoptIdenticalContentMakesNoCommit(t *testing.T) {
	var posts atomic.Int32
	var body string
	client := adoptServer(t, "old", &posts, &body)

	resp := runCreate(t, client, readState("ignored"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if got := posts.Load(); got != 0 {
		t.Fatalf("commit POSTs = %d, want 0 for identical content", got)
	}
	var out filesResourceModel
	if d := resp.State.Get(t.Context(), &out); d.HasError() {
		t.Fatalf("state.Get: %v", d)
	}
	f := out.Files["f.txt"]
	if f.BlobID.ValueString() != "remoteblob" || f.LastCommitID.ValueString() != "remote-lcid" {
		t.Errorf("state must carry the probed blob/lcid, got %q/%q", f.BlobID.ValueString(), f.LastCommitID.ValueString())
	}
	if !out.CommitSHA.IsNull() {
		t.Errorf("commit_sha must be null when no commit was made, got %q", out.CommitSHA.ValueString())
	}
	if out.ID.ValueString() != "proj::main" {
		t.Errorf("id = %q, want proj::main", out.ID.ValueString())
	}
}

// TestCreate_AdoptDifferentContentUpdates: differing remote bytes keep the
// adopt-update.
func TestCreate_AdoptDifferentContentUpdates(t *testing.T) {
	var posts atomic.Int32
	var body string
	client := adoptServer(t, "remote", &posts, &body)

	resp := runCreate(t, client, readState("ignored"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("commit POSTs = %d, want 1", got)
	}
	if !strings.Contains(body, `"action":"update"`) || !strings.Contains(body, `"last_commit_id":"remote-lcid"`) {
		t.Errorf("expected a locked adopt-update, body: %s", body)
	}
}

// TestCreate_AdoptIdenticalBesideNewPathCommitsOnlyTheNewPath: one path that
// already matches the plan and one that does not exist yet make a single
// commit carrying only the create. The adopted path is never probed at the
// new commit; its computed fields come from the adopt probe.
func TestCreate_AdoptIdenticalBesideNewPathCommitsOnlyTheNewPath(t *testing.T) {
	var posts atomic.Int32
	var body string
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		adopted := strings.Contains(r.URL.Path, "a.txt")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "main" && adopted:
			metaHeaders(w, "remoteblob", "remote-lcid", false)
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "main":
			http.Error(w, "absent", http.StatusNotFound)
		case isTreeRequest(r) && !adopted:
			noDirectory(w)
		case r.Method == http.MethodGet && adopted:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fileJSON("a.txt", "remoteblob", "remote-lcid", []byte("same")))
		case r.Method == http.MethodHead && adopted:
			t.Errorf("the adopted path was not committed and must not be probed: %s?%s", r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "newsha":
			stampHeaders(w, "newsha")
		case r.Method == http.MethodPost:
			posts.Add(1)
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"newsha"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	plan := readState("ignored")
	plan.Files["a.txt"] = fileModel{
		Content: types.StringValue("same"), ContentBase64: types.StringNull(),
		BlobID: types.StringNull(), LastCommitID: types.StringNull(), ExecuteFilemode: types.BoolValue(false),
	}
	resp := runCreate(t, client, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("commit POSTs = %d, want 1", got)
	}
	if strings.Contains(body, "a.txt") || !strings.Contains(body, `"action":"create"`) {
		t.Errorf("the commit must create f.txt only, body: %s", body)
	}
	var out filesResourceModel
	if d := resp.State.Get(t.Context(), &out); d.HasError() {
		t.Fatalf("state.Get: %v", d)
	}
	if a := out.Files["a.txt"]; a.BlobID.ValueString() != "remoteblob" || a.LastCommitID.ValueString() != "remote-lcid" {
		t.Errorf("adopted path must carry the probed blob/lcid, got %q/%q", a.BlobID.ValueString(), a.LastCommitID.ValueString())
	}
	if f := out.Files["f.txt"]; f.BlobID.ValueString() != "blob-newsha" || f.LastCommitID.ValueString() != "newsha" {
		t.Errorf("created path must be stamped from the commit, got %q/%q", f.BlobID.ValueString(), f.LastCommitID.ValueString())
	}
	if out.CommitSHA.ValueString() != "newsha" {
		t.Errorf("commit_sha = %q, want newsha", out.CommitSHA.ValueString())
	}
}

// TestUpdate_AdoptIdenticalContentMakesNoCommit is the import round-trip:
// empty state, a plan that matches the repository, no commit, and computed
// fields filled from the probe so the framework sees no unknowns.
func TestUpdate_AdoptIdenticalContentMakesNoCommit(t *testing.T) {
	var posts atomic.Int32
	var body string
	client := adoptServer(t, "old", &posts, &body)

	state := readState("ignored")
	state.Files = map[string]fileModel{}

	resp := runUpdate(t, client, readState("ignored"), state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if got := posts.Load(); got != 0 {
		t.Fatalf("commit POSTs = %d, want 0", got)
	}
	var out filesResourceModel
	if d := resp.State.Get(t.Context(), &out); d.HasError() {
		t.Fatalf("state.Get: %v", d)
	}
	f := out.Files["f.txt"]
	if f.BlobID.ValueString() != "remoteblob" || f.LastCommitID.ValueString() != "remote-lcid" {
		t.Errorf("state must carry the probed blob/lcid, got %q/%q", f.BlobID.ValueString(), f.LastCommitID.ValueString())
	}
	if out.CommitSHA.ValueString() != "sha" {
		t.Errorf("commit_sha must be carried from state, got %q", out.CommitSHA.ValueString())
	}
}

// TestCreate_AdoptIdenticalOnMissingBranchCreatesBranchOnly: identical files
// on the source ref plus a missing target branch means a bare branch
// creation and no commit.
func TestCreate_AdoptIdenticalOnMissingBranchCreatesBranchOnly(t *testing.T) {
	var branchRef string
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
			branchJSON(w, "main", sourceHead)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			http.Error(w, "no branch yet", http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
		case r.Method == http.MethodHead:
			metaHeaders(w, "srcblob", "src-lcid", false)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/files/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fileJSON("f.txt", "srcblob", "src-lcid", []byte("old")))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/branches"):
			var opts gitlab.CreateBranchOptions
			if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
				t.Errorf("decoding the branch body: %v", err)
			}
			branchRef = *opts.Ref
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"name":"feature","commit":{"id":"base"}}`))
		case r.Method == http.MethodPost:
			t.Error("no commit expected when every file already matches")
			http.Error(w, "unexpected", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	plan := readState("ignored")
	plan.Branch = types.StringValue("feature")
	plan.CreateBranchFrom = types.StringValue("main")
	resp := runCreate(t, client, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if branchRef != sourceHead {
		t.Errorf("the branch must still be materialised, from the commit the probes read: ref = %q, want %q", branchRef, sourceHead)
	}
}

// TestCreate_MissingBranchFailureMakesNoCommit: when the branch still has to
// be materialised, a failed re-check under the lock or a failed bare branch
// creation fails Create before any commit, and without state. f.txt exists
// at the source, so no directory check adds a branch lookup before the lock.
func TestCreate_MissingBranchFailureMakesNoCommit(t *testing.T) {
	cases := []struct {
		name string
		// remoteContent is what the source ref holds for f.txt.
		remoteContent string
		recheckStatus int
	}{
		{name: "re-check under the lock fails", remoteContent: "remote", recheckStatus: http.StatusForbidden},
		{name: "bare branch creation fails", remoteContent: "old", recheckStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var branchGets atomic.Int32
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
					branchJSON(w, "main", sourceHead)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
					if branchGets.Add(1) == 1 {
						http.Error(w, "no branch yet", http.StatusNotFound)
						return
					}
					http.Error(w, http.StatusText(tc.recheckStatus), tc.recheckStatus)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":1,"empty_repo":false}`))
				case r.Method == http.MethodHead:
					metaHeaders(w, "srcblob", "src-lcid", false)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/files/"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(fileJSON("f.txt", "srcblob", "src-lcid", []byte(tc.remoteContent)))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/branches"):
					http.Error(w, "forbidden", http.StatusForbidden)
				default:
					t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})

			plan := readState("ignored")
			plan.Branch = types.StringValue("feature")
			plan.CreateBranchFrom = types.StringValue("main")
			resp := runCreate(t, client, plan)
			if !resp.Diagnostics.HasError() {
				t.Fatal("expected Create to fail")
			}
			if !resp.State.Raw.IsNull() {
				t.Errorf("a failed Create must return no state, got: %s", resp.State.Raw)
			}
			if got := branchGets.Load(); got < 2 {
				t.Errorf("branch lookups = %d, want the failure under the lock", got)
			}
		})
	}
}

// TestCreate_AdoptExistingFalseCreatesWithoutProbing: with adopt_existing
// off, Create never probes the branch for existing files; it only checks
// that no directory sits at the path, sends a plain, unlocked create and
// leaves an existing file to fail loudly at GitLab.
func TestCreate_AdoptExistingFalseCreatesWithoutProbing(t *testing.T) {
	var body string
	var committed atomic.Bool
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "newsha":
			stampHeaders(w, "newsha")
		case r.Method == http.MethodHead:
			t.Errorf("adopt_existing = false must not probe the branch: %s?%s", r.URL.Path, r.URL.RawQuery)
			metaHeaders(w, "remoteblob", "remote-lcid", false)
		case isTreeRequest(r):
			noDirectory(w)
		case r.Method == http.MethodPost && !committed.Swap(true):
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"newsha"}`))
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	plan := readState("ignored")
	plan.AdoptExisting = types.BoolValue(false)
	resp := runCreate(t, client, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if !strings.Contains(body, `"action":"create"`) || strings.Contains(body, "last_commit_id") {
		t.Errorf("expected one plain create, body: %s", body)
	}
}

// TestUpdate_StampsOnlyTouchedPaths: after a commit that changed one of two
// files, the untouched file keeps its state values and is never probed. The
// plan carries its computed fields as unknown, so only carrying them over
// from state can fill them.
func TestUpdate_StampsOnlyTouchedPaths(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.Contains(r.URL.Path, "a.txt"):
			t.Errorf("untouched path must not be probed: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		case r.Method == http.MethodHead:
			stampHeaders(w, "upsha")
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"upsha"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	untouched := fileModel{
		Content: types.StringValue("same"), ContentBase64: types.StringNull(),
		BlobID: types.StringValue("blobA"), LastCommitID: types.StringValue("lcidA"), ExecuteFilemode: types.BoolValue(false),
	}
	state := readState("oldblob")
	state.Files["a.txt"] = untouched
	plan := readState("oldblob")
	plan.Files["a.txt"] = untouched
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
	if a := out.Files["a.txt"]; a.BlobID.ValueString() != "blobA" || a.LastCommitID.ValueString() != "lcidA" {
		t.Errorf("untouched file must keep its state values, got %q/%q", a.BlobID.ValueString(), a.LastCommitID.ValueString())
	}
	if f := out.Files["f.txt"]; f.BlobID.ValueString() != "blob-upsha" || f.LastCommitID.ValueString() != "upsha" {
		t.Errorf("touched file must be stamped from the commit, got %q/%q", f.BlobID.ValueString(), f.LastCommitID.ValueString())
	}
}

// TestUpdate_MixedActionsProduceExactlyOneCommit counts the commit POSTs for
// a plan that deletes, creates, updates and chmods at once, and pins the
// actions that one commit carries.
func TestUpdate_MixedActionsProduceExactlyOneCommit(t *testing.T) {
	var posts atomic.Int32
	var sent []string
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isTreeRequest(r):
			noDirectory(w)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			branchJSON(w, "main", "head")
		case r.Method == http.MethodHead:
			if r.URL.Query().Get("ref") == "main" {
				http.Error(w, "absent", http.StatusNotFound)
				return
			}
			stampHeaders(w, "mixsha")
		case r.Method == http.MethodPost:
			posts.Add(1)
			var opts gitlab.CreateCommitOptions
			if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
				t.Errorf("decoding the commit body: %v", err)
			}
			sent = describeActions(opts.Actions)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"mixsha"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	file := func(content string, exec bool) fileModel {
		return fileModel{
			Content: types.StringValue(content), ContentBase64: types.StringNull(),
			BlobID: types.StringValue("b-" + content), LastCommitID: types.StringValue("l-" + content), ExecuteFilemode: types.BoolValue(exec),
		}
	}
	state := readState("oldblob")
	state.Files = map[string]fileModel{"keep.sh": file("k", false), "change.txt": file("old", false), "remove.txt": file("r", false)}
	plan := readState("oldblob")
	plan.Files = map[string]fileModel{"keep.sh": file("k", true), "change.txt": file("new", false), "add.txt": file("a", false)}

	resp := runUpdate(t, client, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("commit POSTs = %d, want exactly 1", got)
	}
	// Deletes come first, then the plan's paths in order, each with the
	// lock token state holds for it.
	want := []string{"delete:remove.txt@l-r", "create:add.txt", "update:change.txt@l-old", "chmod:keep.sh@l-k"}
	if !slices.Equal(sent, want) {
		t.Errorf("commit actions = %q, want %q", sent, want)
	}
}

// TestUpdate_NoOpPreservesUnknownComputedFields drives the zero-action
// branch with every computed field unknown, id included, and asserts state
// ends up fully known and equal to the prior state.
func TestUpdate_NoOpPreservesUnknownComputedFields(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API call %s %s for a no-op update", r.Method, r.URL.Path)
		http.Error(w, "no", http.StatusInternalServerError)
	})
	state := readState("blob")
	plan := readState("blob")
	plan.ID = types.StringUnknown()

	resp := runUpdate(t, client, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	var out filesResourceModel
	if d := resp.State.Get(t.Context(), &out); d.HasError() {
		t.Fatalf("state.Get: %v", d)
	}
	f := out.Files["f.txt"]
	if f.BlobID.ValueString() != "blob" || f.LastCommitID.ValueString() != "oldlcid" || out.CommitSHA.ValueString() != "sha" || out.ID.ValueString() != "proj::main" {
		t.Errorf("computed fields must equal prior state, got blob=%q lcid=%q sha=%q id=%q",
			f.BlobID.ValueString(), f.LastCommitID.ValueString(), out.CommitSHA.ValueString(), out.ID.ValueString())
	}
}

// TestCreate_InvalidFileMakesNoCommit: a file that cannot be turned into an
// action fails before anything reaches the repository.
func TestCreate_InvalidFileMakesNoCommit(t *testing.T) {
	var posts atomic.Int32
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
		case r.Method == http.MethodHead:
			http.Error(w, "absent", http.StatusNotFound)
		case r.Method == http.MethodPost:
			posts.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	plan := readState("ignored")
	plan.Files["f.txt"] = fileModel{
		Content: types.StringNull(), ContentBase64: types.StringValue("not-base64!!!"),
		BlobID: types.StringNull(), LastCommitID: types.StringNull(), ExecuteFilemode: types.BoolValue(false),
	}
	resp := runCreate(t, client, plan)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected invalid base64 to fail Create")
	}
	if got := posts.Load(); got != 0 {
		t.Errorf("commit POSTs = %d, want 0", got)
	}
}

// runImport drives ImportState the way the framework does: an all-null state
// of the resource schema and the composite id.
func runImport(t *testing.T, client *gitlab.Client, id string) *resource.ImportStateResponse {
	t.Helper()
	ctx := t.Context()
	res := newTestResource(client)
	sresp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, sresp)
	// fwserver hands ImportState a null object (EmptyState), not an object
	// of nulls; SetAttribute must create the parent on that shape.
	empty := tftypes.NewValue(sresp.Schema.Type().TerraformType(ctx), nil)
	resp := &resource.ImportStateResponse{State: tfsdk.State{Schema: sresp.Schema, Raw: empty}}
	res.ImportState(ctx, resource.ImportStateRequest{ID: id}, resp)
	if !resp.Diagnostics.HasError() && resp.State.Raw.Equal(empty) {
		t.Fatal("ImportState left the state empty; the framework would report Missing Resource Import State")
	}
	return resp
}

func TestImportState(t *testing.T) {
	branchServer := func(status int) *gitlab.Client {
		return newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/") {
				if status == http.StatusOK {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"base"}}`))
					return
				}
				http.Error(w, "nope", status)
				return
			}
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		})
	}

	t.Run("sets project, branch and id", func(t *testing.T) {
		resp := runImport(t, branchServer(http.StatusOK), "grp/proj::main")
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
		}
		var project, branch, id string
		resp.State.GetAttribute(t.Context(), path.Root("project_id"), &project)
		resp.State.GetAttribute(t.Context(), path.Root("branch"), &branch)
		resp.State.GetAttribute(t.Context(), path.Root("id"), &id)
		if project != "grp/proj" || branch != "main" || id != "grp/proj::main" {
			t.Errorf("imported project/branch/id = %q/%q/%q", project, branch, id)
		}
	})

	t.Run("malformed id", func(t *testing.T) {
		resp := runImport(t, branchServer(http.StatusOK), "no-separator")
		if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "Invalid Import ID" {
			t.Fatalf("expected Invalid Import ID, got %v", resp.Diagnostics)
		}
	})

	t.Run("missing branch", func(t *testing.T) {
		resp := runImport(t, branchServer(http.StatusNotFound), "grp/proj::gone")
		if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "Branch not found" {
			t.Fatalf("expected Branch not found, got %v", resp.Diagnostics)
		}
	})

	t.Run("branch check error", func(t *testing.T) {
		resp := runImport(t, branchServer(http.StatusForbidden), "grp/proj::main")
		if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Summary(), "HTTP 403") {
			t.Fatalf("expected the 403 to surface, got %v", resp.Diagnostics)
		}
	})
}

func TestMissingBranchPreflight_ProjectErrors(t *testing.T) {
	t.Run("project 404 names the project", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no project", http.StatusNotFound)
		})
		_, err := newTestResource(client).missingBranchPreflight(t.Context(), "grp/porject", "main", "")
		if err == nil || !strings.Contains(err.Error(), `project "grp/porject" does not exist or the token cannot see it`) {
			t.Fatalf("want the project diagnostic, got: %v", err)
		}
	})
	t.Run("other project errors surface", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		_, err := newTestResource(client).missingBranchPreflight(t.Context(), "proj", "main", "main")
		if err == nil || !strings.Contains(err.Error(), `checking project "proj"`) {
			t.Fatalf("want a checking-project error, got: %v", err)
		}
	})
}

func TestAdoptAwareActions_EmptyLockTokenErrors(t *testing.T) {
	f := fileModel{Content: types.StringValue("x"), ExecuteFilemode: types.BoolValue(false)}
	probe := remoteProbe{exists: true}
	if _, err := adoptAwareActions("f.txt", f, probe, true); err == nil || !strings.Contains(err.Error(), "optimistic_lock") ||
		strings.Contains(err.Error(), "= false") {
		t.Fatalf("a locked adoption without a token must fail without advising to turn the lock off, got: %v", err)
	}
	actions, err := adoptAwareActions("f.txt", f, probe, false)
	if err != nil || len(actions) != 1 || *actions[0].Action != gitlab.FileUpdate || actions[0].LastCommitID != nil {
		t.Fatalf("an unlocked adoption must be a plain update, got %v / %v", summarise(actions), err)
	}
}

func TestAdoptAwareActions_IdenticalContentChmodOnly(t *testing.T) {
	f := fileModel{Content: types.StringValue("x"), ExecuteFilemode: types.BoolValue(true)}
	probe := remoteProbe{exists: true, lastCommitID: "l", content: []byte("x"), hasContent: true, executeFilemode: false}
	actions, err := adoptAwareActions("f.txt", f, probe, true)
	if err != nil || len(actions) != 1 || *actions[0].Action != gitlab.FileChmod || *actions[0].LastCommitID != "l" {
		t.Fatalf("identical bytes with a differing exec bit must yield a locked chmod only, got %v / %v", summarise(actions), err)
	}
}

func TestTruncateForDiag_RuneBoundary(t *testing.T) {
	// "é" is two bytes; put it across the cut so a byte-wise slice would
	// split it.
	s := strings.Repeat("x", maxDiagBodyChars-1) + "é" + strings.Repeat("y", 50)
	got := truncateForDiag(s)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated diagnostic is not valid UTF-8: %q", got[len(got)-20:])
	}
	if !strings.Contains(got, "truncated") || strings.Contains(got, "é") {
		t.Errorf("expected the rune to be dropped and the suffix appended, got tail %q", got[len(got)-40:])
	}
	if short := "plain"; truncateForDiag(short) != short {
		t.Error("short strings must pass through untouched")
	}
	if got := truncateForDiag("latin1 \x92 quote"); !utf8.ValidString(got) || !strings.Contains(got, "\uFFFD") {
		t.Errorf("invalid bytes must be replaced, got %q", got)
	}
}

// TestDelete_CommitIsNotRetriedOn5xx: a 5xx on the destroy commit may have
// landed, so it is neither replayed nor taken for a rejection: no probe and
// no second commit follow.
func TestDelete_CommitIsNotRetriedOn5xx(t *testing.T) {
	var posts, heads atomic.Int32
	res := newRetryingResource(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			heads.Add(1)
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
	if got := heads.Load(); got != 0 {
		t.Errorf("probes = %d, want none after a 5xx", got)
	}
}
