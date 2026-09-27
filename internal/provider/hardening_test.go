// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// TestDecodeRemoteContent_NilFile: a nil *File, which the client returns for a
// 2xx JSON-null body, must produce an error, not a panic.
func TestDecodeRemoteContent_NilFile(t *testing.T) {
	if _, err := decodeRemoteContent(nil); err == nil {
		t.Fatal("decodeRemoteContent(nil) must return an error, not panic")
	}
}

// TestCrossHostRedirectGuard verifies the token-exfiltration guard: same-host
// redirects (any scheme) pass, off-host redirects, https->http downgrades
// and hops on which net/http turned a POST into a GET are refused by handing
// back the 3xx (ErrUseLastResponse, so the client neither follows nor
// retries), and the chain is capped at 10.
func TestCrossHostRedirectGuard(t *testing.T) {
	mkMethod := func(method, raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return &http.Request{Method: method, URL: u}
	}
	mk := func(raw string) *http.Request { return mkMethod(http.MethodGet, raw) }
	orig := mk("https://gitlab.example.com/api/v4/x")
	post := mkMethod(http.MethodPost, "https://gitlab.example.com/api/v4/projects/p/repository/commits")

	cases := []struct {
		name        string
		req         *http.Request
		via         []*http.Request
		wantRefused bool
	}{
		{"first request", mk("https://gitlab.example.com/y"), nil, false},
		{"same host", mk("https://gitlab.example.com/redir"), []*http.Request{orig}, false},
		{"http to https same host", mk("https://gitlab.example.com/up"), []*http.Request{mk("http://gitlab.example.com/x")}, false},
		{"cross host refused", mk("https://evil.example.net/steal"), []*http.Request{orig}, true},
		{"https to http downgrade refused", mk("http://gitlab.example.com/down"), []*http.Request{orig}, true},
		{"http stays http allowed", mk("http://gitlab.example.com/next"), []*http.Request{mk("http://gitlab.example.com/x")}, false},
		// 301/302/303: net/http re-sends the POST as a body-less GET.
		{"post rewritten to get refused", mk("https://gitlab.example.com/moved/commits"), []*http.Request{post}, true},
		// 307/308: the method and the body are kept.
		{"post kept as post allowed", mkMethod(http.MethodPost, "https://gitlab.example.com/moved/commits"), []*http.Request{post}, false},
		{"head kept as head allowed", mkMethod(http.MethodHead, "https://gitlab.example.com/moved/f"), []*http.Request{mkMethod(http.MethodHead, "https://gitlab.example.com/f")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := crossHostRedirectGuard(c.req, c.via)
			if c.wantRefused != errors.Is(err, http.ErrUseLastResponse) {
				t.Fatalf("wantRefused=%v, got err=%v", c.wantRefused, err)
			}
			if !c.wantRefused && err != nil {
				t.Fatalf("allowed redirect must not error, got %v", err)
			}
		})
	}

	t.Run("too many redirects", func(t *testing.T) {
		via := make([]*http.Request, 10)
		for i := range via {
			via[i] = orig
		}
		if err := crossHostRedirectGuard(mk("https://gitlab.example.com/z"), via); !errors.Is(err, http.ErrUseLastResponse) {
			t.Fatalf("expected the chain to stop after 10 redirects, got %v", err)
		}
	})
}

// TestCrossHostRedirect_EndToEnd drives a real redirect through the
// configured client: the 3xx is surfaced as a refused-redirect diagnostic
// after exactly one request, and the token never reaches the other host.
func TestCrossHostRedirect_EndToEnd(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "" {
			t.Error("the token must never be sent to the redirect target")
		}
		t.Error("the redirect target must not be contacted at all")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, other.URL+"/api/v4/projects/proj/repository/branches/main", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("GITLAB_TOKEN", "")
	resp := runConfigure(t, map[string]tftypes.Value{
		"token":    tftypes.NewValue(tftypes.String, "tok"),
		"base_url": tftypes.NewValue(tftypes.String, srv.URL),
	})
	if resp.Diagnostics.HasError() {
		t.Fatalf("configure: %v", resp.Diagnostics.Errors())
	}
	deps := resp.ResourceData.(*resourceDeps)

	_, _, err := deps.client.Branches.GetBranch("proj", "main", gitlab.WithContext(t.Context()))
	if err == nil {
		t.Fatal("expected the refused redirect to surface as an error")
	}
	summary, detail := apiErrorDiag("reading branch", "proj", "main", err)
	if !strings.Contains(summary, "Refused to follow a GitLab redirect") || !strings.Contains(detail, other.URL) {
		t.Errorf("unexpected diagnostic: %s / %s", summary, detail)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("requests to GitLab = %d, want 1 (a refused redirect must not be retried)", got)
	}
}

// TestSameHostRedirectOnCommit drives a Create through the configured client
// while GitLab redirects the commit POST to another path on the same host.
// net/http re-sends a POST as a GET on 301, 302 and 303, which would read
// the commit list and fail as a JSON decode error; that hop is refused and
// reported as a redirect, once. A 307 or 308 keeps the method and the body
// and is followed: the commit lands once, at the new address.
func TestSameHostRedirectOnCommit(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var posts, moved, listReads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				isCommits := strings.HasSuffix(r.URL.Path, "/repository/commits")
				switch {
				case isCommits && r.Method == http.MethodPost && r.URL.Query().Get("moved") == "":
					posts.Add(1)
					http.Redirect(w, r, r.URL.Path+"?moved=1", status)
				case isCommits && r.Method == http.MethodPost:
					moved.Add(1)
					var opts gitlab.CreateCommitOptions
					if err := json.NewDecoder(r.Body).Decode(&opts); err != nil || len(opts.Actions) != 1 {
						t.Errorf("the followed POST must carry the commit body, got %v (%v)", opts.Actions, err)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":"newsha"}`))
				case isCommits:
					listReads.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`[]`))
				case r.Method == http.MethodHead && r.URL.Query().Get("ref") == "newsha":
					stampHeaders(w, "newsha")
				case r.Method == http.MethodHead:
					http.Error(w, "404 File Not Found", http.StatusNotFound)
				case isTreeRequest(r):
					noDirectory(w)
				case strings.Contains(r.URL.Path, "/repository/branches/"):
					branchJSON(w, "main", "head")
				default:
					t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			t.Cleanup(srv.Close)

			t.Setenv("GITLAB_TOKEN", "")
			cfg := runConfigure(t, map[string]tftypes.Value{
				"token":    tftypes.NewValue(tftypes.String, "tok"),
				"base_url": tftypes.NewValue(tftypes.String, srv.URL),
			})
			if cfg.Diagnostics.HasError() {
				t.Fatalf("configure: %v", cfg.Diagnostics.Errors())
			}
			deps := cfg.ResourceData.(*resourceDeps)
			res := &filesResource{client: deps.client, locks: deps.locks, retryCommits: deps.retryCommits}

			plan := readState("")
			plan.Files["f.txt"] = fileModel{Content: types.StringValue("new"), ContentBase64: types.StringNull(),
				ExecuteFilemode: types.BoolValue(false)}
			resp := runCreateOn(t, res, plan)

			if posts.Load() != 1 || listReads.Load() != 0 {
				t.Errorf("commit POSTs = %d, commit list reads = %d, want 1 and 0", posts.Load(), listReads.Load())
			}
			if status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect {
				if resp.Diagnostics.HasError() || moved.Load() != 1 {
					t.Fatalf("a %d keeps the POST: want one commit at the new address, got %d and %v", status, moved.Load(), resp.Diagnostics)
				}
				return
			}
			errs := resp.Diagnostics.Errors()
			if len(errs) != 1 || errs[0].Summary() != fmt.Sprintf("Refused to follow a GitLab redirect (HTTP %d)", status) {
				t.Fatalf("want the refused-redirect error, got %v", resp.Diagnostics)
			}
			if d := errs[0].Detail(); !strings.Contains(d, "?moved=1") || !strings.Contains(d, "write request into a GET") {
				t.Errorf("detail must name the target and the reason, got: %s", d)
			}
			if moved.Load() != 0 {
				t.Errorf("the redirect target must not receive the commit, got %d", moved.Load())
			}
		})
	}
}

// TestNullBodyDecodesToNilCommit documents the precondition the Create/Update
// nil-guards defend against: client-go decodes a 2xx JSON-null body into
// a nil *Commit with no error. If a future client-go changes this, the guards can
// be revisited.
func TestNullBodyDecodesToNilCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("null"))
	}))
	defer srv.Close()

	client, err := gitlab.NewClient("tok", gitlab.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	commit, _, err := client.Commits.CreateCommit("proj", &gitlab.CreateCommitOptions{
		Branch:        new("main"),
		CommitMessage: new("m"),
		Actions:       []*gitlab.CommitActionOptions{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if commit != nil {
		t.Fatalf("expected nil commit from null body, got %+v", commit)
	}
}

// TestBranchHeadDataSource_NoHeadCommit drives the branch_head data source
// against a server that returns a branch without a usable head commit (a
// null or omitted commit, or one with no id), asserting a clean error
// diagnostic instead of a nil-deref panic or an empty commit_sha.
func TestBranchHeadDataSource_NoHeadCommit(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{name: "null commit", body: `{"name":"main","commit":null,"protected":false}`},
		{name: "omitted commit", body: `{"name":"main","protected":false}`},
		{name: "commit without id", body: `{"name":"main","commit":{},"protected":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newReadClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})

			resp, _ := runBranchHeadDataSourceRead(t, client)
			if !resp.Diagnostics.HasError() {
				t.Fatal("expected an error diagnostic for a branch with no head commit, got none")
			}
			if got := resp.Diagnostics.Errors()[0].Summary(); got != "GitLab returned a branch with no commit" {
				t.Errorf("summary = %q, want %q", got, "GitLab returned a branch with no commit")
			}
		})
	}
}

// TestDiffActions_AdoptForwardsLockToken: when adopt_existing rewrites a
// new-to-state path that already exists remotely into an update, the probed
// last_commit_id is forwarded under optimistic_lock so the overwrite is still
// guarded against a concurrent writer; with optimistic_lock off, no token is sent.
func TestDiffActions_AdoptForwardsLockToken(t *testing.T) {
	const probedLCID = "abc123probed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Adopt content compare: remote differs from the plan.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fileJSON("adopted.txt", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", probedLCID, []byte("remote")))
			return
		}
		if r.Method != http.MethodHead {
			http.Error(w, "expected HEAD", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Gitlab-Blob-Id", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
		w.Header().Set("X-Gitlab-Last-Commit-Id", probedLCID)
		w.Header().Set("X-Gitlab-File-Path", "adopted.txt")
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

	emptyState := func() filesResourceModel { return filesResourceModel{Files: map[string]fileModel{}} }
	plan := func(lock bool) filesResourceModel {
		return filesResourceModel{
			ProjectID:      types.StringValue("proj"),
			Branch:         types.StringValue("main"),
			OptimisticLock: types.BoolValue(lock),
			AdoptExisting:  types.BoolValue(true),
			Files:          map[string]fileModel{"adopted.txt": {Content: types.StringValue("hello")}},
		}
	}

	t.Run("lock on forwards probed token", func(t *testing.T) {
		actions, _, diags := res.diffActions(t.Context(), plan(true), emptyState())
		if diags.HasError() {
			t.Fatalf("diffActions: %v", diags)
		}
		if len(actions) != 1 {
			t.Fatalf("want 1 action, got %d", len(actions))
		}
		a := actions[0]
		if *a.Action != gitlab.FileUpdate {
			t.Errorf("action = %s, want update (adopt rewrite)", *a.Action)
		}
		if a.LastCommitID == nil || *a.LastCommitID != probedLCID {
			t.Errorf("LastCommitID = %v, want %q (probed token forwarded)", a.LastCommitID, probedLCID)
		}
	})

	t.Run("lock off omits token", func(t *testing.T) {
		actions, _, diags := res.diffActions(t.Context(), plan(false), emptyState())
		if diags.HasError() {
			t.Fatalf("diffActions: %v", diags)
		}
		if len(actions) != 1 {
			t.Fatalf("want 1 action, got %d", len(actions))
		}
		if actions[0].LastCommitID != nil {
			t.Errorf("LastCommitID = %q, want nil (lock disabled)", *actions[0].LastCommitID)
		}
	})
}

// TestDiffActions_AdoptEmitsChmodOnExecMismatch: the commits API ignores
// execute_filemode on update actions, so adopting an existing file whose
// remote exec bit differs from the plan must add a companion chmod to the
// same action set; matching bits must not.
func TestDiffActions_AdoptEmitsChmodOnExecMismatch(t *testing.T) {
	const probedLCID = "abc123probed"
	remoteExec := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fileJSON("tool.sh", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", probedLCID, []byte("remote")))
			return
		}
		if r.Method != http.MethodHead {
			http.Error(w, "expected HEAD", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Gitlab-Blob-Id", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
		w.Header().Set("X-Gitlab-Last-Commit-Id", probedLCID)
		w.Header().Set("X-Gitlab-File-Path", "tool.sh")
		w.Header().Set("X-Gitlab-Ref", "main")
		w.Header().Set("X-Gitlab-Size", "5")
		w.Header().Set("X-Gitlab-Execute-Filemode", strconv.FormatBool(remoteExec))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := gitlab.NewClient("tok", gitlab.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res := &filesResource{client: client}

	plan := func(exec bool) filesResourceModel {
		return filesResourceModel{
			ProjectID:      types.StringValue("proj"),
			Branch:         types.StringValue("main"),
			OptimisticLock: types.BoolValue(true),
			AdoptExisting:  types.BoolValue(true),
			Files: map[string]fileModel{"tool.sh": {
				Content:         types.StringValue("#!/bin/sh\n"),
				ExecuteFilemode: types.BoolValue(exec),
			}},
		}
	}
	emptyState := filesResourceModel{Files: map[string]fileModel{}}

	t.Run("mismatch adds chmod with lock token", func(t *testing.T) {
		actions, _, diags := res.diffActions(t.Context(), plan(true), emptyState)
		if diags.HasError() {
			t.Fatalf("diffActions: %v", diags)
		}
		if len(actions) != 2 {
			t.Fatalf("want update+chmod, got %d actions", len(actions))
		}
		if *actions[0].Action != gitlab.FileUpdate {
			t.Errorf("first action = %s, want update", *actions[0].Action)
		}
		chmod := actions[1]
		if *chmod.Action != gitlab.FileChmod {
			t.Fatalf("second action = %s, want chmod", *chmod.Action)
		}
		if chmod.ExecuteFilemode == nil || !*chmod.ExecuteFilemode {
			t.Error("chmod must set execute_filemode=true")
		}
		if chmod.LastCommitID == nil || *chmod.LastCommitID != probedLCID {
			t.Errorf("chmod LastCommitID = %v, want %q", chmod.LastCommitID, probedLCID)
		}
	})

	t.Run("matching bit emits no chmod", func(t *testing.T) {
		actions, _, diags := res.diffActions(t.Context(), plan(false), emptyState)
		if diags.HasError() {
			t.Fatalf("diffActions: %v", diags)
		}
		if len(actions) != 1 {
			t.Fatalf("want a single update, got %d actions", len(actions))
		}
	})

	t.Run("remote exec true plan false adds chmod false", func(t *testing.T) {
		remoteExec = true
		defer func() { remoteExec = false }()
		actions, _, diags := res.diffActions(t.Context(), plan(false), emptyState)
		if diags.HasError() {
			t.Fatalf("diffActions: %v", diags)
		}
		if len(actions) != 2 {
			t.Fatalf("want update+chmod, got %d actions", len(actions))
		}
		chmod := actions[1]
		if *chmod.Action != gitlab.FileChmod || chmod.ExecuteFilemode == nil || *chmod.ExecuteFilemode {
			t.Error("chmod must clear execute_filemode when the remote bit is set and the plan wants it off")
		}
	})
}

// TestTreeEntryWithoutPath: a tree listing that answers with a null entry or
// an entry without a path fails the file it was listed for, in the one-entry
// directory check and in the recursive listing of a directory turning into
// a file alike, and nothing is committed. Read as "no directory" it would let
// a create replace a directory; dereferenced it would panic the provider.
func TestTreeEntryWithoutPath(t *testing.T) {
	for _, body := range []string{`[null]`, `[{"path":"","type":"blob"}]`} {
		for _, recursive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s recursive %v", body, recursive), func(t *testing.T) {
				var posts atomic.Int32
				client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
						branchJSON(w, "main", "base")
					case r.Method == http.MethodHead:
						http.Error(w, "404 File Not Found", http.StatusNotFound)
					case isTreeRequest(r) && recursive && r.URL.Query().Get("recursive") != "true":
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`[{"path":"conf/app.yaml","type":"blob"}]`))
					case isTreeRequest(r):
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(body))
					case r.Method == http.MethodPost:
						posts.Add(1)
						http.Error(w, "unexpected commit", http.StatusInternalServerError)
					default:
						t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
						http.Error(w, "unexpected", http.StatusInternalServerError)
					}
				})
				var diags diag.Diagnostics
				if recursive {
					diags = runUpdate(t, client, managedFiles(true, map[string]string{"conf": ""}),
						managedFiles(true, map[string]string{"conf/app.yaml": "l1"})).Diagnostics
				} else {
					diags = runCreate(t, client, managedFiles(true, map[string]string{"conf": ""})).Diagnostics
				}
				wantDiag(t, "error", diags.Errors(), "GitLab returned a tree entry without a path")
				if errs := diags.Errors(); len(errs) == 1 {
					withPath, ok := errs[0].(diag.DiagnosticWithPath)
					if !ok || !withPath.Path().Equal(path.Root("files").AtMapKey("conf")) {
						t.Errorf("the error must point at files[\"conf\"], got %v", errs[0])
					}
				}
				if posts.Load() != 0 {
					t.Errorf("commits = %d, want none", posts.Load())
				}
			})
		}
	}
}
