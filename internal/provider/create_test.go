// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// decodeCommit reads a commit request body, failing the test on a malformed
// one; it only calls t.Error, so handlers may use it.
func decodeCommit(t *testing.T, r *http.Request) gitlab.CreateCommitOptions {
	t.Helper()
	var opts gitlab.CreateCommitOptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		t.Errorf("decoding the commit body: %v", err)
	}
	return opts
}

// TestCreate_StartSHACarriesNoLockToken: with start_sha and no start_branch,
// GitLab checks last_commit_id against the default branch instead of the
// base commit the probes read, so the first commit of a branch created from
// create_branch_from carries no token, on the adopt-update and on its
// companion chmod alike, whether the source is a branch or a commit SHA.
func TestCreate_StartSHACarriesNoLockToken(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, from := range []string{"main", sha} {
		t.Run(from, func(t *testing.T) {
			base := sourceHead
			if from == sha {
				base = sha
			}
			var mu sync.Mutex
			var commits []gitlab.CreateCommitOptions
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				ref := r.URL.Query().Get("ref")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
					branchJSON(w, "main", sourceHead)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
					http.Error(w, "no branch yet", http.StatusNotFound)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
					projectJSON(w, false)
				case r.Method == http.MethodHead && ref == base:
					// The base holds f.txt with other content and the exec bit set.
					metaHeaders(w, "srcblob", "src-lcid", true)
				case r.Method == http.MethodGet && ref == base:
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(fileJSON("f.txt", "srcblob", "src-lcid", []byte("remote")))
				case r.Method == http.MethodHead && ref == "newsha":
					stampHeaders(w, "newsha")
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/commits"):
					opts := decodeCommit(t, r)
					mu.Lock()
					commits = append(commits, opts)
					mu.Unlock()
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
			plan.CreateBranchFrom = types.StringValue(from)
			if resp := runCreate(t, client, plan); resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			if len(commits) != 1 {
				t.Fatalf("commits = %d, want 1", len(commits))
			}
			c := commits[0]
			if c.StartSHA == nil || *c.StartSHA != base || c.StartBranch != nil {
				t.Errorf("start_sha / start_branch = %v / %v, want %s / none", c.StartSHA, c.StartBranch, base)
			}
			if got, want := describeActions(c.Actions), []string{"update:f.txt", "chmod:f.txt"}; !slices.Equal(got, want) {
				t.Errorf("actions = %q, want %q (no last_commit_id)", got, want)
			}
		})
	}
}

// TestCreate_BranchCreatedMeanwhileIsProbedAgain: when another instance
// created the branch between the first probe and the lock, Create probes the
// branch again under the lock and commits against it with the branch's
// tokens and no start_sha. Here the source held f.txt as planned, but the
// branch the other instance created holds other content.
func TestCreate_BranchCreatedMeanwhileIsProbedAgain(t *testing.T) {
	var featureGets atomic.Int32
	var mu sync.Mutex
	var commits []gitlab.CreateCommitOptions
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("ref")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
			branchJSON(w, "main", sourceHead)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/feature"):
			if featureGets.Add(1) == 1 {
				http.Error(w, "no branch yet", http.StatusNotFound)
				return
			}
			branchJSON(w, "feature", "sibling")
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
			projectJSON(w, false)
		case r.Method == http.MethodHead && ref == sourceHead:
			metaHeaders(w, "srcblob", "src-lcid", false)
		case r.Method == http.MethodGet && ref == sourceHead:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fileJSON("f.txt", "srcblob", "src-lcid", []byte("old")))
		case r.Method == http.MethodHead && ref == "feature":
			metaHeaders(w, "branchblob", "branch-lcid", false)
		case r.Method == http.MethodGet && ref == "feature":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fileJSON("f.txt", "branchblob", "branch-lcid", []byte("sibling")))
		case r.Method == http.MethodHead && ref == "newsha":
			stampHeaders(w, "newsha")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/commits"):
			opts := decodeCommit(t, r)
			mu.Lock()
			commits = append(commits, opts)
			mu.Unlock()
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
	plan.CreateBranchFrom = types.StringValue("main")
	if resp := runCreate(t, client, plan); resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if len(commits) != 1 {
		t.Fatalf("commits = %d, want 1: the branch holds other content than the plan", len(commits))
	}
	c := commits[0]
	if c.StartSHA != nil || c.StartBranch != nil {
		t.Errorf("a commit to the existing branch must not carry start_sha or start_branch, got %v / %v", c.StartSHA, c.StartBranch)
	}
	if got, want := describeActions(c.Actions), []string{"update:f.txt@branch-lcid"}; !slices.Equal(got, want) {
		t.Errorf("actions = %q, want %q", got, want)
	}
}

// TestCreate_SourceBranchMovingAfterTheProbeIsNotUsed: create_branch_from is
// resolved to its head commit once; the probes read that commit and the new
// branch starts from it, even when another resource moves the source branch
// before the commit (here, during the second lookup of the target, which
// follows the probes: the one confirming new.txt's directory check, or the
// locked re-check). State then describes what the new branch holds. The
// commit and the bare branch creation are both covered.
func TestCreate_SourceBranchMovingAfterTheProbeIsNotUsed(t *testing.T) {
	const moved = "5eed00000000000000000000000000000000b2b2"
	for _, withNew := range []bool{true, false} {
		t.Run(fmt.Sprintf("with a file to create %v", withNew), func(t *testing.T) {
			var mu sync.Mutex
			head := sourceHead
			var featureGets int
			var startSHA, branchRef *string
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				ref := r.URL.Query().Get("ref")
				// cfg.txt reads "X" at the old head and "Y" once main has moved.
				content, lcid := "X", "c0"
				if ref == moved || (ref == "main" && head == moved) {
					content, lcid = "Y", "c1"
				}
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
					branchJSON(w, "main", head)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/staging"):
					if featureGets++; featureGets == 2 {
						head = moved
					}
					http.Error(w, "no branch yet", http.StatusNotFound)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
					projectJSON(w, false)
				case r.Method == http.MethodHead && ref == "newsha":
					stampHeaders(w, "newsha")
				case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/cfg.txt"):
					metaHeaders(w, "blob-"+content, lcid, false)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cfg.txt"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(fileJSON("cfg.txt", "blob-"+content, lcid, []byte(content)))
				case r.Method == http.MethodHead:
					http.Error(w, "absent", http.StatusNotFound)
				case isTreeRequest(r):
					noDirectory(w)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/branches"):
					var opts gitlab.CreateBranchOptions
					if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
						t.Errorf("decoding the branch body: %v", err)
					}
					branchRef = opts.Ref
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"name":"staging","commit":{"id":"x"}}`))
				case r.Method == http.MethodPost:
					opts := decodeCommit(t, r)
					startSHA = opts.StartSHA
					if opts.StartBranch != nil {
						t.Errorf("start_branch %q sent; the source must be pinned to a commit", *opts.StartBranch)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":"newsha"}`))
				default:
					t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})

			plan := readState("ignored")
			plan.Branch = types.StringValue("staging")
			plan.CreateBranchFrom = types.StringValue("main")
			plan.Files = map[string]fileModel{"cfg.txt": plan.Files["f.txt"]}
			plan = withContent(plan, map[string]string{"cfg.txt": "X"})
			if withNew {
				plan.Files["new.txt"] = plan.Files["cfg.txt"]
			}
			resp := runCreate(t, client, plan)
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			mu.Lock()
			defer mu.Unlock()
			if head != moved {
				t.Fatal("the source branch never moved; the test does not exercise the race")
			}
			got := startSHA
			if !withNew {
				got = branchRef
			}
			if got == nil || *got != sourceHead {
				t.Errorf("the new branch must start from the commit the probes read (%s), got %v", sourceHead, got)
			}
			var out filesResourceModel
			if d := resp.State.Get(t.Context(), &out); d.HasError() {
				t.Fatalf("state.Get: %v", d)
			}
			if cfg := out.Files["cfg.txt"]; cfg.BlobID.ValueString() != "blob-X" || cfg.LastCommitID.ValueString() != "c0" {
				t.Errorf("cfg.txt state = %q/%q, want the base's blob-X/c0", cfg.BlobID.ValueString(), cfg.LastCommitID.ValueString())
			}
		})
	}
}

// TestAdoptProbeFailureFailsBeforeCommit: a probe of a path to adopt that
// fails, or answers with ids that cannot be used, fails Create and Update on
// that file before anything is committed, with GitLab's own status in the
// diagnostic. Falling back to a create would fail on "already exists" and
// hide the cause; falling back to an update would commit content that may
// already match, or record a mode GitLab does not have.
func TestAdoptProbeFailureFailsBeforeCommit(t *testing.T) {
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, http.StatusText(code), code) }
	}
	meta := func(blob, lcid string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { metaHeaders(w, blob, lcid, true) }
	}
	content := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fileJSON("f.txt", "remoteblob", "remote-lcid", []byte("old")))
	}
	cases := []struct {
		head, get http.HandlerFunc
		name      string
		want      string
		update    bool
		lock      bool
	}{
		{name: "metadata 502", head: status(http.StatusBadGateway), lock: true, want: "HTTP 502"},
		{name: "content 502 after the metadata", head: meta("remoteblob", "remote-lcid"), get: status(http.StatusBadGateway), lock: true, want: "HTTP 502"},
		{name: "content 429 after import", update: true, head: meta("remoteblob", "remote-lcid"), get: status(http.StatusTooManyRequests), lock: true, want: "HTTP 429"},
		{name: "metadata without ids, lock off", head: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, get: content, want: "no blob_id or last_commit_id"},
		{name: "oversized last_commit_id", head: meta("remoteblob", strings.Repeat("c", 300)), get: content, lock: true, want: "last_commit_id of unexpected length 300"},
		{name: "oversized blob_id after import", update: true, head: meta(strings.Repeat("b", 300), "remote-lcid"), get: content, want: "blob_id of unexpected length 300"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
					branchJSON(w, "main", "base")
				case r.Method == http.MethodHead:
					tc.head(w, r)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/files/") && tc.get != nil:
					tc.get(w, r)
				case r.Method == http.MethodPost:
					posts.Add(1)
					http.Error(w, "unexpected commit", http.StatusInternalServerError)
				default:
					t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})
			plan := readState("ignored")
			plan.OptimisticLock = types.BoolValue(tc.lock)
			var diags diag.Diagnostics
			if tc.update {
				state := plan
				state.Files = map[string]fileModel{}
				diags = runUpdate(t, client, plan, state).Diagnostics
			} else {
				diags = runCreate(t, client, plan).Diagnostics
			}
			if posts.Load() != 0 {
				t.Errorf("commits = %d, want none", posts.Load())
			}
			if len(diags.Errors()) != 1 {
				t.Fatalf("want one error, got %v", diags)
			}
			d := diags.Errors()[0]
			withPath, ok := d.(diag.DiagnosticWithPath)
			if !ok || !withPath.Path().Equal(path.Root("files").AtMapKey("f.txt")) {
				t.Errorf("the error must point at files[\"f.txt\"], got %v", d)
			}
			if text := d.Summary() + " " + d.Detail(); !strings.Contains(text, tc.want) || !strings.Contains(text, "Nothing was committed.") {
				t.Errorf("diagnostic must carry %q and say nothing was committed, got %q", tc.want, text)
			}
		})
	}
}

// TestCreate_DirectoryInTheWayFails: GitLab lets a created file replace a
// directory with everything in it, so a path that holds a directory is never
// created, whether adoption is on or off.
func TestCreate_DirectoryInTheWayFails(t *testing.T) {
	for _, adopt := range []bool{true, false} {
		t.Run(fmt.Sprintf("adopt_existing %v", adopt), func(t *testing.T) {
			fake := &repoFake{files: map[string]string{"conf/extra.yaml": "l0"}}
			plan := managedFiles(true, map[string]string{"conf": ""})
			plan.AdoptExisting = types.BoolValue(adopt)
			resp := runCreate(t, fake.client(t), plan)
			wantDiag(t, "error", resp.Diagnostics.Errors(), `"conf" is a directory on branch "main" and holds "conf/extra.yaml"`)
			if commits, _, files := fake.recorded(); len(commits) != 0 || len(files) != 1 {
				t.Errorf("commits = %q, files = %v; want none and the directory untouched", commits, files)
			}
		})
	}
}

// TestDirectoryCheck404NeedsTheBranchLookup: GitLab answers a tree listing
// with 404 when Gitaly fails to resolve the ref, as for a path that is not a
// directory, so the answer is trusted only once a branch lookup has
// answered too. When that lookup fails, Create and Update commit nothing.
func TestDirectoryCheck404NeedsTheBranchLookup(t *testing.T) {
	for _, update := range []bool{false, true} {
		name := "create"
		if update {
			name = "update adding the path"
		}
		t.Run(name, func(t *testing.T) {
			var branchGets, posts atomic.Int32
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
					// Create's own branch check comes first and succeeds.
					if n := branchGets.Add(1); !update && n == 1 {
						branchJSON(w, "main", "base")
						return
					}
					http.Error(w, "gitaly unavailable", http.StatusServiceUnavailable)
				case r.Method == http.MethodHead:
					http.Error(w, "404 File Not Found", http.StatusNotFound)
				case isTreeRequest(r):
					http.Error(w, `{"message":"404 Tree Not Found"}`, http.StatusNotFound)
				case r.Method == http.MethodPost:
					posts.Add(1)
					http.Error(w, "unexpected commit", http.StatusInternalServerError)
				default:
					t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})
			plan := managedFiles(true, map[string]string{"conf": ""})
			var diags diag.Diagnostics
			if update {
				state := managedFiles(true, nil)
				diags = runUpdate(t, client, plan, state).Diagnostics
			} else {
				diags = runCreate(t, client, plan).Diagnostics
			}
			wantDiag(t, "error", diags.Errors(), "a 404 is trusted only once the branch lookup succeeds. Nothing was committed.")
			if posts.Load() != 0 {
				t.Errorf("commits = %d, want none", posts.Load())
			}
		})
	}
}

// TestUpdate_DirectoryToFile: a directory turns into a file within one
// commit only when the commit deletes every file in it first; a file the
// resource does not manage, found on any page of the listing, fails the
// update instead of being wiped with the directory.
func TestUpdate_DirectoryToFile(t *testing.T) {
	state := managedFiles(true, map[string]string{"conf/app.yaml": "l1"})
	plan := managedFiles(true, map[string]string{"conf": ""})

	t.Run("managed files only", func(t *testing.T) {
		fake := &repoFake{files: map[string]string{"conf/app.yaml": "l1", "other.txt": "l2"}}
		resp := runUpdate(t, fake.client(t), plan, state)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
		}
		commits, _, files := fake.recorded()
		if want := [][]string{{"delete:conf/app.yaml@l1", "create:conf"}}; !slices.EqualFunc(commits, want, slices.Equal) {
			t.Errorf("commits = %q, want %q", commits, want)
		}
		if _, ok := files["conf"]; !ok || len(files) != 2 {
			t.Errorf("files = %v, want conf and other.txt", files)
		}
	})

	t.Run("an unmanaged file on the second page", func(t *testing.T) {
		var posts atomic.Int32
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			switch {
			case r.Method == http.MethodHead:
				http.Error(w, "404 File Not Found", http.StatusNotFound)
			case isTreeRequest(r) && q.Get("recursive") != "true":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"path":"conf/app.yaml","type":"blob"}]`))
			case isTreeRequest(r) && q.Get("page") == "":
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Next-Page", "2")
				_, _ = w.Write([]byte(`[{"path":"conf/app.yaml","type":"blob"},{"path":"conf/sub","type":"tree"}]`))
			case isTreeRequest(r) && q.Get("page") == "2":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"path":"conf/sub/extra.yaml","type":"blob"}]`))
			case r.Method == http.MethodPost:
				posts.Add(1)
				http.Error(w, "unexpected commit", http.StatusInternalServerError)
			default:
				t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}
		})
		resp := runUpdate(t, client, plan, state)
		wantDiag(t, "error", resp.Diagnostics.Errors(), `"conf" is a directory on branch "main" and holds "conf/sub/extra.yaml"`)
		if posts.Load() != 0 {
			t.Errorf("commits = %d, want none", posts.Load())
		}
	})

	t.Run("listing fails", func(t *testing.T) {
		client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodHead:
				http.Error(w, "404 File Not Found", http.StatusNotFound)
			case isTreeRequest(r):
				http.Error(w, "forbidden", http.StatusForbidden)
			default:
				t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}
		})
		resp := runUpdate(t, client, plan, state)
		wantDiag(t, "error", resp.Diagnostics.Errors(), "Nothing was committed.")
	})

	t.Run("a file in it left in place for another resource", func(t *testing.T) {
		fake := &repoFake{files: map[string]string{"conf/app.yaml": "l1"}}
		res := newTestResource(fake.client(t))
		res.locks.claim("proj", "main", []string{"conf/app.yaml"})
		resp := runUpdateOn(t, res, plan, state)
		wantDiag(t, "error", resp.Diagnostics.Errors(), `it would replace the directory holding "conf/app.yaml"`)
		if commits, _, files := fake.recorded(); len(commits) != 0 || len(files) != 1 {
			t.Errorf("commits = %q, files = %v; want none and the file kept", commits, files)
		}
	})
}

// TestCreate_BareBranchCreationReplayedAfter5xx: client-go replays the
// branch POST after a 5xx, and when GitLab had already created the branch
// the replay answers "Branch already exists". The branch is there, so Create
// succeeds without a commit and logs a warning; when the branch is not
// there, the error stands.
func TestCreate_BareBranchCreationReplayedAfter5xx(t *testing.T) {
	for _, created := range []bool{true, false} {
		t.Run(fmt.Sprintf("branch created %v", created), func(t *testing.T) {
			var branchPosts, commitPosts atomic.Int32
			var exists atomic.Bool
			res := newRetryingResource(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
					branchJSON(w, "main", sourceHead)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/feature"):
					if !exists.Load() {
						http.Error(w, "no branch yet", http.StatusNotFound)
						return
					}
					branchJSON(w, "feature", sourceHead)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
					projectJSON(w, false)
				case r.Method == http.MethodHead:
					metaHeaders(w, "srcblob", "src-lcid", false)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/files/"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(fileJSON("f.txt", "srcblob", "src-lcid", []byte("old")))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/branches"):
					if branchPosts.Add(1) == 1 {
						exists.Store(created)
						http.Error(w, "bad gateway", http.StatusBadGateway)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"message":"Branch already exists"}`))
				case r.Method == http.MethodPost:
					commitPosts.Add(1)
					http.Error(w, "unexpected commit", http.StatusInternalServerError)
				default:
					t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})

			plan := readState("ignored")
			plan.Branch = types.StringValue("feature")
			plan.CreateBranchFrom = types.StringValue("main")
			var logs bytes.Buffer
			ctx := tflogtest.RootLogger(t.Context(), &logs)
			req, resp := createRequest(t, res, plan)
			res.Create(ctx, req, resp)
			checkCreateResult(t, resp)

			if got := branchPosts.Load(); got != 2 {
				t.Errorf("branch POSTs = %d, want the request and its replay", got)
			}
			if got := commitPosts.Load(); got != 0 {
				t.Errorf("commits = %d, want none", got)
			}
			if !created {
				wantDiag(t, "error", resp.Diagnostics.Errors(), "Branch already exists")
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			var out filesResourceModel
			if d := resp.State.Get(t.Context(), &out); d.HasError() {
				t.Fatalf("state.Get: %v", d)
			}
			if !out.CommitSHA.IsNull() {
				t.Errorf("commit_sha = %q, want null", out.CommitSHA.ValueString())
			}
			entries, err := tflogtest.MultilineJSONDecode(&logs)
			if err != nil {
				t.Fatalf("decoding logs: %v", err)
			}
			if !slices.ContainsFunc(entries, func(e map[string]any) bool {
				return e["@level"] == "warn" && e["@message"] == "Branch creation reported an error, but the branch exists"
			}) {
				t.Errorf("want a warning about the replayed branch creation, logs: %v", entries)
			}
		})
	}
}

// TestFileActionErrorReadsTheSameInCreateAndUpdate: a file that cannot be
// turned into a commit action is reported identically by Create and by an
// Update that adds it, on the file's attribute path.
func TestFileActionErrorReadsTheSameInCreateAndUpdate(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			branchJSON(w, "main", "base")
		case r.Method == http.MethodHead:
			http.Error(w, "404 File Not Found", http.StatusNotFound)
		case isTreeRequest(r):
			noDirectory(w)
		default:
			t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	plan := readState("ignored")
	plan.Files["f.txt"] = fileModel{
		Content: types.StringNull(), ContentBase64: types.StringValue("not-base64!!!"),
		BlobID: types.StringNull(), LastCommitID: types.StringNull(), ExecuteFilemode: types.BoolValue(false),
	}
	state := readState("ignored")
	state.Files = map[string]fileModel{}

	created := runCreate(t, client, plan).Diagnostics
	updated := runUpdate(t, client, plan, state).Diagnostics
	if len(created) != 1 || len(updated) != 1 {
		t.Fatalf("want one diagnostic each, got %v and %v", created, updated)
	}
	if !created[0].Equal(updated[0]) {
		t.Errorf("Create and Update report the same failure differently:\n%v\n%v", created[0], updated[0])
	}
	if withPath, ok := created[0].(diag.DiagnosticWithPath); !ok || !withPath.Path().Equal(path.Root("files").AtMapKey("f.txt")) {
		t.Errorf("the error must point at files[\"f.txt\"], got %v", created[0])
	}
}

// TestCreate_LogsTheActionCount: the commit log line counts actions, which
// differ from files as soon as an adoption adds a companion chmod.
func TestCreate_LogsTheActionCount(t *testing.T) {
	var posts atomic.Int32
	var body string
	res := newTestResource(adoptServer(t, "remote", &posts, &body))
	plan := readState("ignored")
	f := plan.Files["f.txt"]
	f.ExecuteFilemode = types.BoolValue(true)
	plan.Files["f.txt"] = f

	var logs bytes.Buffer
	ctx := tflogtest.RootLogger(t.Context(), &logs)
	req, resp := createRequest(t, res, plan)
	res.Create(ctx, req, resp)
	checkCreateResult(t, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	entries, err := tflogtest.MultilineJSONDecode(&logs)
	if err != nil {
		t.Fatalf("decoding logs: %v", err)
	}
	i := slices.IndexFunc(entries, func(e map[string]any) bool { return e["@message"] == "GitLab files commit created" })
	if i < 0 {
		t.Fatalf("no commit log line in %v", entries)
	}
	if got, stale := entries[i]["actions"], entries[i]["files"]; got != float64(2) || stale != nil {
		t.Errorf("log fields actions = %v, files = %v; want 2 actions (update and chmod) and no files field", got, stale)
	}
}

// TestMissingBranchPreflight_ResolvesBase pins the commit a missing branch
// starts from: a commit SHA as it is (lowercased), a branch by its head, and
// no commit at all on an empty repository without create_branch_from.
func TestMissingBranchPreflight_ResolvesBase(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name, from, branchBody, want, wantErr string
		branchStatus                          int
		empty                                 bool
	}{
		{name: "commit SHA", from: strings.ToUpper(sha), want: sha},
		{name: "branch head", from: "main", branchBody: `{"name":"main","commit":{"id":"` + sourceHead + `"}}`, want: sourceHead},
		{name: "missing branch", from: "v1.0", branchStatus: http.StatusNotFound, wantErr: "tags are not supported"},
		{name: "branch lookup fails", from: "main", branchStatus: http.StatusForbidden, wantErr: `resolving create_branch_from branch "main"`},
		{name: "branch without a head", from: "main", branchBody: `{"name":"main","commit":null}`, wantErr: "no head commit"},
		{name: "branch head without an id", from: "main", branchBody: `{"name":"main","commit":{"id":""}}`, wantErr: "no head commit"},
		{name: "empty repository", empty: true, want: ""},
		{name: "empty repository with a source", from: "main", empty: true, wantErr: "add depends_on on that resource so its " +
			"commit lands first (or apply again once the branch exists); remove create_branch_from only if this resource " +
			"itself should make the first commit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/proj"):
					projectJSON(w, tc.empty)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/") && tc.branchStatus != 0:
					http.Error(w, http.StatusText(tc.branchStatus), tc.branchStatus)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/") && tc.branchBody != "":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.branchBody))
				default:
					t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})
			got, err := newTestResource(client).missingBranchPreflight(t.Context(), "proj", "feature", tc.from)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("want an error with %q, got %q, %v", tc.wantErr, got, err)
				}
			case err != nil || got != tc.want:
				t.Errorf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestCheckFileIDs(t *testing.T) {
	long := strings.Repeat("a", maxBlobIDLen+1)
	cases := []struct {
		blob, lcid, want string
	}{
		{blob: "b", lcid: "l"},
		{blob: strings.Repeat("a", maxBlobIDLen), lcid: strings.Repeat("a", maxBlobIDLen)},
		{blob: "", lcid: "l", want: "no blob_id or last_commit_id"},
		{blob: "b", lcid: "", want: "no blob_id or last_commit_id"},
		{blob: long, lcid: "l", want: "blob_id of unexpected length 257"},
		{blob: "b", lcid: long, want: "last_commit_id of unexpected length 257"},
	}
	for _, tc := range cases {
		err := checkFileIDs(&gitlab.File{BlobID: tc.blob, LastCommitID: tc.lcid})
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("checkFileIDs(%d-char blob, %d-char lcid) = %v, want %q", len(tc.blob), len(tc.lcid), err, tc.want)
		}
	}
}

// TestRead_UnusableMetadataIDsError: a metadata response whose ids cannot be
// compared or stored fails the refresh rather than reading as "unchanged" or
// landing in state.
func TestRead_UnusableMetadataIDsError(t *testing.T) {
	for _, tc := range []struct{ name, blob, lcid string }{
		{name: "no last_commit_id", blob: "blob"},
		{name: "oversized last_commit_id", blob: "blob", lcid: strings.Repeat("c", 300)},
		{name: "oversized blob_id", blob: strings.Repeat("b", 300), lcid: "lcid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.Header().Set("X-Gitlab-Blob-Id", tc.blob)
					if tc.lcid != "" {
						w.Header().Set("X-Gitlab-Last-Commit-Id", tc.lcid)
					}
					w.WriteHeader(http.StatusOK)
					return
				}
				t.Errorf("unexpected call %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			})
			resp, _ := runRead(t, client, readState("blob"))
			if !resp.Diagnostics.HasError() {
				t.Fatal("expected the refresh to fail")
			}
		})
	}
}
