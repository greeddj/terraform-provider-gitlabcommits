// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// operation is one resource instance's Create, Update or Delete. It is built
// on the test goroutine, since building the request may t.Fatalf, and run
// from any goroutine.
type operation struct {
	run   func(ctx context.Context)
	diags func() diag.Diagnostics
	state func() tfsdk.State
}

func createOp(t *testing.T, res *filesResource, plan filesResourceModel) operation {
	t.Helper()
	req, resp := createRequest(t, res, plan)
	return operation{
		run: func(ctx context.Context) {
			res.Create(ctx, req, resp)
			checkCreateResult(t, resp)
		},
		diags: func() diag.Diagnostics { return resp.Diagnostics },
		state: func() tfsdk.State { return resp.State },
	}
}

func updateOp(t *testing.T, res *filesResource, plan, state filesResourceModel) operation {
	t.Helper()
	req, resp := updateRequest(t, res, plan, state)
	return operation{
		run: func(ctx context.Context) {
			res.Update(ctx, req, resp)
			checkUpdateResult(t, req, resp)
		},
		diags: func() diag.Diagnostics { return resp.Diagnostics },
		state: func() tfsdk.State { return resp.State },
	}
}

func deleteOp(t *testing.T, res *filesResource, state filesResourceModel) operation {
	t.Helper()
	req, resp := deleteRequest(t, res, state)
	return operation{
		run:   func(ctx context.Context) { res.Delete(ctx, req, resp) },
		diags: func() diag.Diagnostics { return resp.Diagnostics },
		state: func() tfsdk.State { return resp.State },
	}
}

// withContent sets the content of every path in content on m.
func withContent(m filesResourceModel, content map[string]string) filesResourceModel {
	for p, c := range content {
		f := m.Files[p]
		f.Content = types.StringValue(c)
		m.Files[p] = f
	}
	return m
}

// checkStateMatchesRepo asserts that every file a resource's state records
// is on the branch at the last_commit_id state holds for it: state never
// claims a file GitLab does not have.
func checkStateMatchesRepo(t *testing.T, who string, st tfsdk.State, repo map[string]string) {
	t.Helper()
	var m filesResourceModel
	if d := st.Get(t.Context(), &m); d.HasError() {
		t.Fatalf("%s: state.Get: %v", who, d)
	}
	for p, f := range m.Files {
		if lcid, ok := repo[p]; !ok || lcid != f.LastCommitID.ValueString() {
			t.Errorf("%s: state records %q at %q, but the branch has it at %q (present: %v)", who, p, f.LastCommitID.ValueString(), lcid, ok)
		}
	}
}

// waitFor polls cond until it holds, failing the test after a few seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestBranchLocks_Claims: a claim is kept per branch and path, with every
// project_id spelling it was made under.
func TestBranchLocks_Claims(t *testing.T) {
	locks := newBranchLocks()
	locks.claim("proj", "main", []string{"a.txt", "b.txt"})
	locks.claim("123", "main", []string{"a.txt"})
	locks.claim("proj", "other", []string{"c.txt"})

	for _, c := range []struct {
		branch, path string
		want         []string
	}{
		{"main", "a.txt", []string{"123", "proj"}},
		{"main", "b.txt", []string{"proj"}},
		{"main", "c.txt", nil},
		{"other", "c.txt", []string{"proj"}},
		{"other", "a.txt", nil},
	} {
		if got := locks.claimants(c.branch, c.path); !slices.Equal(got, c.want) {
			t.Errorf("claimants(%q, %q) = %q, want %q", c.branch, c.path, got, c.want)
		}
	}
}

// TestClaims_AdoptionWaitsForADeleteInFlight: when one resource drops a path
// and another takes it over in the same apply, Terraform may run both at
// once. The receiver must not probe while the giver's delete commit is in
// flight: it would find the file, adopt it without a commit and record it,
// and the commit would then remove it. It claims the path and passes through
// the branch lock first, so it finds the file gone and creates it again.
func TestClaims_AdoptionWaitsForADeleteInFlight(t *testing.T) {
	for _, c := range []struct {
		name            string
		giverUpdates    bool
		receiverUpdates bool
	}{
		{name: "create taking over from a destroy"},
		{name: "update taking over from a destroy", receiverUpdates: true},
		{name: "update taking over from another update", giverUpdates: true, receiverUpdates: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := &repoFake{files: map[string]string{"f.txt": "l1", "g.txt": "l2", "z.txt": "l3"}}
			inFlight, release := make(chan struct{}), make(chan struct{})
			var gated atomic.Bool
			fake.beforeCommit = func() {
				if gated.CompareAndSwap(false, true) {
					close(inFlight)
					<-release
				}
			}
			client := fake.client(t)
			locks := newBranchLocks()
			giverRes := &filesResource{client: client, locks: locks}
			receiverRes := &filesResource{client: client, locks: locks}

			var giver, receiver operation
			if c.giverUpdates {
				giver = updateOp(t, giverRes, managedFiles(true, map[string]string{"g.txt": "l2"}),
					managedFiles(true, map[string]string{"f.txt": "l1", "g.txt": "l2"}))
			} else {
				giver = deleteOp(t, giverRes, managedFiles(true, map[string]string{"f.txt": "l1"}))
			}
			if c.receiverUpdates {
				receiver = updateOp(t, receiverRes, managedFiles(true, map[string]string{"f.txt": "", "z.txt": "l3"}),
					managedFiles(true, map[string]string{"z.txt": "l3"}))
			} else {
				receiver = createOp(t, receiverRes, managedFiles(true, map[string]string{"f.txt": ""}))
			}

			var wg sync.WaitGroup
			var releaseOnce sync.Once
			defer func() {
				releaseOnce.Do(func() { close(release) })
				wg.Wait()
			}()
			wg.Go(func() { giver.run(t.Context()) })
			select {
			case <-inFlight:
			case <-time.After(5 * time.Second):
				t.Fatal("the giver never sent its delete commit")
			}
			wg.Go(func() { receiver.run(t.Context()) })
			waitFor(t, "the receiver's claim", func() bool { return len(locks.claimants("main", "f.txt")) > 0 })
			// A receiver that probed without waiting for the lock does so now,
			// while the delete commit is still held.
			time.Sleep(50 * time.Millisecond)
			releaseOnce.Do(func() { close(release) })
			wg.Wait()

			commits, _, repo := fake.recorded()
			want := [][]string{{"delete:f.txt@l1"}, {"create:f.txt"}}
			if !slices.EqualFunc(commits, want, slices.Equal[[]string]) {
				t.Errorf("commits = %q, want %q", commits, want)
			}
			wantDiag(t, "giver diagnostic", giver.diags(), "")
			wantDiag(t, "receiver diagnostic", receiver.diags(), "")
			checkStateMatchesRepo(t, "receiver", receiver.state(), repo)
			if _, ok := repo["f.txt"]; !ok {
				t.Error("f.txt is gone from the branch although the receiver now manages it")
			}
		})
	}
}

// TestClaims_CreateBeforeDestroyKeepsAdoptedFiles: under create_before_destroy
// a replacement on the same branch creates the new object first, which adopts
// the old object's files, and destroys the old object afterwards. That
// destroy must leave every path the new object took over in place and still
// delete the ones only the old object managed. In the default order the
// destroy runs first and the create puts the files back.
func TestClaims_CreateBeforeDestroyKeepsAdoptedFiles(t *testing.T) {
	cases := []struct {
		oldTokens    map[string]string
		newContent   map[string]string
		name         string
		wantWarning  string
		wantCommits  [][]string
		destroyFirst bool
	}{
		{
			name:        "identical content makes no commit",
			oldTokens:   map[string]string{"f.txt": "l1"},
			newContent:  map[string]string{"f.txt": "x"},
			wantWarning: `"f.txt"`,
		},
		{
			name:        "changed content lands only the adoption",
			oldTokens:   map[string]string{"f.txt": "l1"},
			newContent:  map[string]string{"f.txt": "changed"},
			wantCommits: [][]string{{"update:f.txt@l1"}},
			wantWarning: `"f.txt"`,
		},
		{
			name:        "a path only the old object managed is still deleted",
			oldTokens:   map[string]string{"f.txt": "l1", "old.txt": "l2"},
			newContent:  map[string]string{"f.txt": "x"},
			wantCommits: [][]string{{"delete:old.txt@l2"}},
			wantWarning: `"f.txt"`,
		},
		{
			name:         "destroy first deletes and creates again",
			oldTokens:    map[string]string{"f.txt": "l1"},
			newContent:   map[string]string{"f.txt": "x"},
			destroyFirst: true,
			wantCommits:  [][]string{{"delete:f.txt@l1"}, {"create:f.txt"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &repoFake{files: maps.Clone(c.oldTokens)}
			res := newTestResource(fake.client(t))
			newTokens := map[string]string{}
			for p := range c.newContent {
				newTokens[p] = ""
			}
			create := createOp(t, res, withContent(managedFiles(true, newTokens), c.newContent))
			destroy := deleteOp(t, res, managedFiles(true, c.oldTokens))

			if c.destroyFirst {
				destroy.run(t.Context())
				create.run(t.Context())
			} else {
				create.run(t.Context())
				destroy.run(t.Context())
			}

			commits, _, repo := fake.recorded()
			if !slices.EqualFunc(commits, c.wantCommits, slices.Equal[[]string]) {
				t.Errorf("commits = %q, want %q", commits, c.wantCommits)
			}
			wantDiag(t, "create diagnostic", create.diags(), "")
			wantDiag(t, "destroy error", destroy.diags().Errors(), "")
			wantDiag(t, "destroy warning", destroy.diags().Warnings(), c.wantWarning)
			checkStateMatchesRepo(t, "new object", create.state(), repo)
			if _, ok := repo["old.txt"]; ok {
				t.Error("old.txt, managed only by the old object, was not deleted")
			}
		})
	}
}

// TestClaims_PathMovedBetweenResources: a file moved from one resource's
// files map to another's on the same branch. Applied receiver first, the
// receiver adopts it without a commit and the giver must then leave it in
// place, committing only its other changes; applied giver first, the file is
// deleted and created again.
func TestClaims_PathMovedBetweenResources(t *testing.T) {
	cases := []struct {
		name          string
		wantWarning   string
		wantCommits   [][]string
		receiverFirst bool
		giverChanges  bool
	}{
		{
			name:          "receiver first keeps the file without a commit",
			receiverFirst: true,
			wantWarning:   `"f.txt"`,
		},
		{
			name:          "receiver first, the giver commits only its other change",
			receiverFirst: true,
			giverChanges:  true,
			wantCommits:   [][]string{{"update:g.txt@l2"}},
			wantWarning:   `"f.txt"`,
		},
		{
			name:        "giver first deletes it and the receiver creates it again",
			wantCommits: [][]string{{"delete:f.txt@l1"}, {"create:f.txt"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &repoFake{files: map[string]string{"f.txt": "l1", "g.txt": "l2", "z.txt": "l3"}}
			res := newTestResource(fake.client(t))
			giverPlan := managedFiles(true, map[string]string{"g.txt": "l2"})
			if c.giverChanges {
				giverPlan = withContent(giverPlan, map[string]string{"g.txt": "changed"})
			}
			giver := updateOp(t, res, giverPlan, managedFiles(true, map[string]string{"f.txt": "l1", "g.txt": "l2"}))
			receiver := updateOp(t, res, managedFiles(true, map[string]string{"f.txt": "", "z.txt": "l3"}),
				managedFiles(true, map[string]string{"z.txt": "l3"}))

			if c.receiverFirst {
				receiver.run(t.Context())
				giver.run(t.Context())
			} else {
				giver.run(t.Context())
				receiver.run(t.Context())
			}

			commits, _, repo := fake.recorded()
			if !slices.EqualFunc(commits, c.wantCommits, slices.Equal[[]string]) {
				t.Errorf("commits = %q, want %q", commits, c.wantCommits)
			}
			wantDiag(t, "receiver diagnostic", receiver.diags(), "")
			wantDiag(t, "giver error", giver.diags().Errors(), "")
			wantDiag(t, "giver warning", giver.diags().Warnings(), c.wantWarning)
			checkStateMatchesRepo(t, "receiver", receiver.state(), repo)
			checkStateMatchesRepo(t, "giver", giver.state(), repo)
			var g filesResourceModel
			if d := giver.state().Get(t.Context(), &g); d.HasError() {
				t.Fatalf("state.Get: %v", d)
			}
			if _, kept := g.Files["f.txt"]; kept {
				t.Error("the giver's state must no longer manage f.txt")
			}
			if len(c.wantCommits) == 0 && g.CommitSHA.ValueString() != "sha" {
				t.Errorf("the giver made no commit, so commit_sha must stay as it was, got %q", g.CommitSHA.ValueString())
			}
		})
	}
}

// TestClaims_AnotherSpellingOfTheProject: a replacement that respells
// project_id ("proj" -> "123") under create_before_destroy claims the paths
// under the new spelling. The old object's destroy, under the old one,
// resolves both: "123" is the numeric ID itself, "proj" takes one lookup.
// The same ID keeps the file, a different one deletes it, and a failed
// lookup fails the operation without a commit, naming both spellings and the
// path and warning that simply applying again would delete the file. Two
// path spellings are looked up once each and cached for the process.
func TestClaims_AnotherSpellingOfTheProject(t *testing.T) {
	cases := []struct {
		projects      map[string]int64
		name          string
		claimant      string
		wantWarning   string
		wantError     string
		wantCommits   [][]string
		wantLookups   []string
		projectStatus int
		giverUpdates  bool
	}{
		{
			name:        "the same project keeps the file",
			claimant:    "123",
			projects:    map[string]int64{"proj": 123},
			wantWarning: `project_id "123", the same project as "proj"`,
			wantLookups: []string{"proj"},
		},
		{
			name:        "another project deletes it",
			claimant:    "123",
			projects:    map[string]int64{"proj": 7},
			wantCommits: [][]string{{"delete:f.txt@l1"}},
			wantLookups: []string{"proj"},
		},
		{
			name:        "two path spellings of one project are both looked up",
			claimant:    "alias",
			projects:    map[string]int64{"proj": 7, "alias": 7},
			wantWarning: `project_id "alias", the same project as "proj"`,
			wantLookups: []string{"proj", "alias"},
		},
		{
			name:          "a failed lookup commits nothing",
			claimant:      "123",
			projects:      map[string]int64{},
			projectStatus: http.StatusInternalServerError,
			wantError:     `"f.txt" on branch "main" is to be deleted through project_id "proj"`,
			wantLookups:   []string{"proj"},
		},
		{
			name:          "a failed lookup fails an update too",
			claimant:      "123",
			projects:      map[string]int64{},
			projectStatus: http.StatusInternalServerError,
			giverUpdates:  true,
			wantError:     `resource in this run adopted or wrote that path through project_id "123"`,
			wantLookups:   []string{"proj"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &repoFake{
				files:         map[string]string{"f.txt": "l1", "g.txt": "l2"},
				projects:      c.projects,
				projectStatus: c.projectStatus,
			}
			res := newTestResource(fake.client(t))
			respelled := managedFiles(true, map[string]string{"f.txt": ""})
			respelled.ProjectID = types.StringValue(c.claimant)
			create := createOp(t, res, respelled)
			var old operation
			if c.giverUpdates {
				old = updateOp(t, res, managedFiles(true, map[string]string{"g.txt": "l2"}),
					managedFiles(true, map[string]string{"f.txt": "l1", "g.txt": "l2"}))
			} else {
				old = deleteOp(t, res, managedFiles(true, map[string]string{"f.txt": "l1"}))
			}

			create.run(t.Context())
			old.run(t.Context())

			commits, _, repo := fake.recorded()
			if !slices.EqualFunc(commits, c.wantCommits, slices.Equal[[]string]) {
				t.Errorf("commits = %q, want %q", commits, c.wantCommits)
			}
			if got := fake.projectLookups(); !slices.Equal(got, c.wantLookups) {
				t.Errorf("project lookups = %q, want %q", got, c.wantLookups)
			}
			wantDiag(t, "create diagnostic", create.diags(), "")
			wantDiag(t, "error", old.diags().Errors(), c.wantError)
			wantDiag(t, "warning", old.diags().Warnings(), c.wantWarning)
			if c.wantError != "" {
				if _, ok := repo["f.txt"]; !ok {
					t.Error("a failed lookup must leave f.txt in place")
				}
				if errs := old.diags().Errors(); len(errs) == 1 {
					for _, want := range []string{
						`project_id "proj"`, `project_id "123"`, `"f.txt"`,
						"Whether the two name the same project is unknown",
						"do not simply apply again: a later run cannot see this run's claim",
						"terraform state rm",
					} {
						if !strings.Contains(errs[0].Detail(), want) {
							t.Errorf("error detail %q does not mention %q", errs[0].Detail(), want)
						}
					}
				}
			}
			if c.wantWarning == "" {
				return
			}
			checkStateMatchesRepo(t, "new object", create.state(), repo)
			again := deleteOp(t, res, managedFiles(true, map[string]string{"f.txt": "l1"}))
			again.run(t.Context())
			if got := fake.projectLookups(); !slices.Equal(got, c.wantLookups) {
				t.Errorf("project lookups after a second destroy = %q, want %q (cached)", got, c.wantLookups)
			}
		})
	}
}

// TestClaims_NoLookupWithoutAnotherSpelling: claims on another branch or
// another path, or under the destroy's own spelling, never need a project
// lookup; the fake fails any lookup as an unexpected call.
func TestClaims_NoLookupWithoutAnotherSpelling(t *testing.T) {
	fake := &repoFake{files: map[string]string{"f.txt": "l1", "h.txt": "l3"}}
	res := newTestResource(fake.client(t))
	res.locks.claim("123", "other", []string{"f.txt"})
	res.locks.claim("123", "main", []string{"g.txt"})
	res.locks.claim("123", "main", []string{"h.txt"})
	res.locks.claim("proj", "main", []string{"h.txt"})

	resp := runDeleteOn(t, res, managedFiles(true, map[string]string{"f.txt": "l1", "h.txt": "l3"}))

	commits, _, repo := fake.recorded()
	want := [][]string{{"delete:f.txt@l1"}}
	if !slices.EqualFunc(commits, want, slices.Equal[[]string]) {
		t.Errorf("commits = %q, want %q", commits, want)
	}
	if _, ok := repo["h.txt"]; !ok {
		t.Error("h.txt is claimed under this spelling and must stay")
	}
	wantDiag(t, "error", resp.Diagnostics.Errors(), "")
	wantDiag(t, "warning", resp.Diagnostics.Warnings(), `"h.txt"`)
}

// TestClaims_AllDigitSpellingsNeedNoLookup: GitLab resolves an all-digit
// project_id by ID, so two of them are compared as numbers without a
// request. Resources for different projects that share a branch name and a
// path (the same .gitlab-ci.yml on main everywhere) must not make a destroy
// depend on a lookup, and "0123" names project 123.
func TestClaims_AllDigitSpellingsNeedNoLookup(t *testing.T) {
	fake := &repoFake{files: map[string]string{"a.txt": "l1", "b.txt": "l2"}}
	res := newTestResource(fake.client(t))
	res.locks.claim("456", "main", []string{"a.txt"})
	res.locks.claim("0123", "main", []string{"b.txt"})
	state := managedFiles(true, map[string]string{"a.txt": "l1", "b.txt": "l2"})
	state.ProjectID = types.StringValue("123")

	resp := runDeleteOn(t, res, state)

	commits, _, repo := fake.recorded()
	want := [][]string{{"delete:a.txt@l1"}}
	if !slices.EqualFunc(commits, want, slices.Equal[[]string]) {
		t.Errorf("commits = %q, want %q", commits, want)
	}
	if _, ok := repo["b.txt"]; !ok {
		t.Error(`b.txt is claimed under "0123", the same project as "123", and must stay`)
	}
	wantDiag(t, "error", resp.Diagnostics.Errors(), "")
	wantDiag(t, "warning", resp.Diagnostics.Warnings(), `project_id "0123", the same project as "123"`)
}

// TestClaims_DestroyFallbackLeavesClaimedFilesAlone: when GitLab rejects a
// destroy commit, Delete probes and retries only the deletes it actually
// sent. A path left in place for another resource is neither probed nor
// warned about again, and when every other file turns out gone the warning
// says the kept files are not among them.
func TestClaims_DestroyFallbackLeavesClaimedFilesAlone(t *testing.T) {
	fake := &repoFake{files: map[string]string{"f.txt": "l1"}}
	res := newTestResource(fake.client(t))
	res.locks.claim("proj", "main", []string{"f.txt"})

	resp := runDeleteOn(t, res, managedFiles(true, map[string]string{"f.txt": "l1", "old.txt": "l2"}))

	commits, probes, repo := fake.recorded()
	if want := [][]string{{"delete:old.txt@l2"}}; !slices.EqualFunc(commits, want, slices.Equal[[]string]) {
		t.Errorf("commits = %q, want %q", commits, want)
	}
	if want := []string{"old.txt@main"}; !slices.Equal(probes, want) {
		t.Errorf("probes = %q, want %q", probes, want)
	}
	if _, ok := repo["f.txt"]; !ok {
		t.Error("f.txt is claimed by another resource and must stay")
	}
	wantDiag(t, "error", resp.Diagnostics.Errors(), "")
	warnings := resp.Diagnostics.Warnings()
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want the claim warning and the nothing-left warning", warnings)
	}
	if w := warnings[0]; w.Summary() != "File left in place for another resource" || !strings.Contains(w.Detail(), `"f.txt"`) {
		t.Errorf("first warning = %q: %q, want the claim warning for f.txt", w.Summary(), w.Detail())
	}
	if w := warnings[1]; w.Summary() != "Nothing left to delete" ||
		!strings.HasPrefix(w.Detail(), "apart from the files left in place for another resource, none of the managed files exists") {
		t.Errorf("second warning = %q: %q, want it to set the kept files apart", w.Summary(), w.Detail())
	}
}

// TestClaims_CheckedUnderTheBranchLock: a destroy checks the claims only once
// it holds the branch lock, right before its commit, so a path claimed while
// it waited for the lock is still left in place.
func TestClaims_CheckedUnderTheBranchLock(t *testing.T) {
	fake := &repoFake{files: map[string]string{"f.txt": "l1"}}
	res := newTestResource(fake.client(t))
	destroy := deleteOp(t, res, managedFiles(true, map[string]string{"f.txt": "l1"}))
	release, err := res.locks.acquire(t.Context(), "proj", "main")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	var wg sync.WaitGroup
	wg.Go(func() { destroy.run(t.Context()) })
	// Let the destroy build its actions and queue for the lock.
	time.Sleep(20 * time.Millisecond)
	res.locks.claim("proj", "main", []string{"f.txt"})
	release()
	wg.Wait()

	commits, _, repo := fake.recorded()
	if len(commits) != 0 {
		t.Errorf("commits = %q, want none", commits)
	}
	if _, ok := repo["f.txt"]; !ok {
		t.Error("f.txt was claimed before the destroy got the lock and must stay")
	}
	wantDiag(t, "warning", destroy.diags().Warnings(), "adopted or wrote \"f.txt\"")
}

// TestClaims_ProbesWaitForTheBranchLock: Create and an Update that adds a
// path claim it and pass through the branch lock before they probe it, so
// while another commit holds the lock nothing is probed. A claim is kept when
// the operation then fails.
func TestClaims_ProbesWaitForTheBranchLock(t *testing.T) {
	for _, update := range []bool{false, true} {
		name := "create"
		if update {
			name = "update adding a path"
		}
		t.Run(name, func(t *testing.T) {
			fake := &repoFake{files: map[string]string{"f.txt": "l1", "z.txt": "l3"}}
			res := newTestResource(fake.client(t))
			op := createOp(t, res, managedFiles(true, map[string]string{"f.txt": ""}))
			if update {
				op = updateOp(t, res, managedFiles(true, map[string]string{"f.txt": "", "z.txt": "l3"}),
					managedFiles(true, map[string]string{"z.txt": "l3"}))
			}
			release, err := res.locks.acquire(t.Context(), "proj", "main")
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			defer release()

			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			op.run(ctx)

			wantDiag(t, "error", op.diags().Errors(), "Cancelled while waiting for the branch lock")
			if commits, probes, _ := fake.recorded(); len(probes) != 0 || len(commits) != 0 {
				t.Errorf("probes = %q, commits = %q, want none before the lock", probes, commits)
			}
			if got := res.locks.claimants("main", "f.txt"); !slices.Equal(got, []string{"proj"}) {
				t.Errorf("claimants of f.txt = %q, want the failed operation's claim kept", got)
			}
			if got := res.locks.claimants("main", "z.txt"); update && len(got) != 0 {
				t.Errorf("an update claims only the paths it adds, got a claim on z.txt: %q", got)
			}
		})
	}
}

// TestClaims_WarningNamesThePath pins the warning text a destroy gives for a
// file it leaves to another resource.
func TestClaims_WarningNamesThePath(t *testing.T) {
	fake := &repoFake{files: map[string]string{"dir/f.txt": "l1"}}
	res := newTestResource(fake.client(t))
	res.locks.claim("proj", "main", []string{"dir/f.txt"})

	resp := runDeleteOn(t, res, managedFiles(true, map[string]string{"dir/f.txt": "l1"}))

	warnings := resp.Diagnostics.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one", warnings)
	}
	w := warnings[0]
	for _, want := range []string{`"dir/f.txt"`, `branch "main"`, "another gitlabcommits_files resource in this run adopted or wrote", "not deleted"} {
		if !strings.Contains(w.Detail(), want) {
			t.Errorf("warning detail %q does not mention %q", w.Detail(), want)
		}
	}
}

// TestClaims_CreateOverADirectoryFilledMeanwhile: a resource that creates a
// file where another resource in the same apply adds a file inside a
// directory of that name would replace the directory, and wipe that file,
// if the other commit lands after its directory check. The other resource
// claimed its path before committing, so the claim, checked under the
// branch lock, fails the create before it is sent. Here the other resource
// runs to completion while the directory check's answer is on its way.
func TestClaims_CreateOverADirectoryFilledMeanwhile(t *testing.T) {
	for _, update := range []bool{false, true} {
		name := "create"
		if update {
			name = "update adding the path"
		}
		t.Run(name, func(t *testing.T) {
			fake := &repoFake{files: map[string]string{"a.txt": "l0"}}
			locks := newBranchLocks()
			var filler operation
			var filled atomic.Bool
			h := fake.handler(t)
			client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
				h(w, r)
				// The listing's answer is written but reaches the resource
				// only once this handler returns.
				if isTreeRequest(r) && r.URL.Query().Get("path") == "conf" && filled.CompareAndSwap(false, true) {
					filler.run(r.Context())
				}
			})
			fillerRes := &filesResource{client: client, locks: locks}
			res := &filesResource{client: client, locks: locks}
			filler = createOp(t, fillerRes, managedFiles(true, map[string]string{"conf/x": ""}))
			op := createOp(t, res, managedFiles(true, map[string]string{"conf": ""}))
			if update {
				op = updateOp(t, res, managedFiles(true, map[string]string{"a.txt": "l0", "conf": ""}),
					managedFiles(true, map[string]string{"a.txt": "l0"}))
			}

			op.run(t.Context())

			if !filled.Load() {
				t.Fatal("the directory was never checked; the test does not exercise the race")
			}
			commits, _, repo := fake.recorded()
			if want := [][]string{{"create:conf/x"}}; !slices.EqualFunc(commits, want, slices.Equal[[]string]) {
				t.Errorf("commits = %q, want only the filler's %q", commits, want)
			}
			wantDiag(t, "filler diagnostic", filler.diags(), "")
			wantDiag(t, "error", op.diags().Errors(), `"conf" cannot be created on branch "main": it would replace the directory holding "conf/x"`)
			checkStateMatchesRepo(t, "filler", filler.state(), repo)
		})
	}
}

// TestClaims_CreateOverAClaimedDirectory: a claim inside the directory a
// create would replace fails the create when it names the same project,
// under any spelling, and is ignored when it names another project that
// shares the branch name. A failed lookup commits nothing.
func TestClaims_CreateOverAClaimedDirectory(t *testing.T) {
	cases := []struct {
		projects      map[string]int64
		name          string
		claimant      string
		wantError     string
		wantCommits   [][]string
		projectStatus int
	}{
		{
			name:      "the same spelling",
			claimant:  "proj",
			wantError: `which a gitlabcommits_files resource in this run adopts or writes, so nothing was committed`,
		},
		{
			name:      "another spelling of the same project",
			claimant:  "123",
			projects:  map[string]int64{"proj": 123},
			wantError: `adopts or writes through project_id "123", the same project as "proj", so nothing was committed`,
		},
		{
			name:        "another project",
			claimant:    "7",
			projects:    map[string]int64{"proj": 123},
			wantCommits: [][]string{{"create:conf"}},
		},
		{
			name:          "a failed lookup",
			claimant:      "7",
			projects:      map[string]int64{},
			projectStatus: http.StatusInternalServerError,
			wantError:     "Whether the two name the same project is unknown",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &repoFake{files: map[string]string{}, projects: c.projects, projectStatus: c.projectStatus}
			res := newTestResource(fake.client(t))
			res.locks.claim(c.claimant, "main", []string{"conf/sub/x"})
			res.locks.claim("proj", "other", []string{"conf/y"})

			resp := runCreateOn(t, res, managedFiles(true, map[string]string{"conf": ""}))

			commits, _, _ := fake.recorded()
			if !slices.EqualFunc(commits, c.wantCommits, slices.Equal[[]string]) {
				t.Errorf("commits = %q, want %q", commits, c.wantCommits)
			}
			wantDiag(t, "error", resp.Diagnostics.Errors(), c.wantError)
			if errs := resp.Diagnostics.Errors(); len(errs) == 1 {
				withPath, ok := errs[0].(diag.DiagnosticWithPath)
				if !ok || !withPath.Path().Equal(path.Root("files").AtMapKey("conf")) {
					t.Errorf("the error must point at files[\"conf\"], got %v", errs[0])
				}
				if !strings.Contains(errs[0].Detail(), `"conf/sub/x"`) {
					t.Errorf("the error must name the claimed path, got %q", errs[0].Detail())
				}
			}
		})
	}
}
