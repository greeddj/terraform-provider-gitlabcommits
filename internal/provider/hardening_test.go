// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// TestDecodeRemoteContent_NilFile pins the DOS-3 guard: a nil *File (which the
// client returns for a 2xx JSON-null body) must produce an error, not a panic.
func TestDecodeRemoteContent_NilFile(t *testing.T) {
	if _, err := decodeRemoteContent(nil); err == nil {
		t.Fatal("decodeRemoteContent(nil) must return an error, not panic")
	}
}

// TestCrossHostRedirectGuard verifies the token-exfiltration guard: same-host
// redirects (any scheme) pass, off-host redirects are refused, and the chain is
// capped at 10.
func TestCrossHostRedirectGuard(t *testing.T) {
	mk := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return &http.Request{URL: u}
	}
	orig := mk("https://gitlab.example.com/api/v4/x")

	cases := []struct {
		name    string
		req     *http.Request
		via     []*http.Request
		wantErr bool
	}{
		{"first request", mk("https://gitlab.example.com/y"), nil, false},
		{"same host", mk("https://gitlab.example.com/redir"), []*http.Request{orig}, false},
		{"http to https same host", mk("https://gitlab.example.com/up"), []*http.Request{mk("http://gitlab.example.com/x")}, false},
		{"cross host refused", mk("https://evil.example.net/steal"), []*http.Request{orig}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := crossHostRedirectGuard(c.req, c.via)
			if c.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got err=%v", c.wantErr, err)
			}
		})
	}

	t.Run("too many redirects", func(t *testing.T) {
		via := make([]*http.Request, 10)
		for i := range via {
			via[i] = orig
		}
		if err := crossHostRedirectGuard(mk("https://gitlab.example.com/z"), via); err == nil {
			t.Fatal("expected error after 10 redirects")
		}
	})
}

// TestNullBodyDecodesToNilCommit documents the precondition the Create/Update
// nil-guards (DOS-1) defend against: client-go decodes a 2xx JSON-null body into
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
		Branch:        gitlab.Ptr("main"),
		CommitMessage: gitlab.Ptr("m"),
		Actions:       []*gitlab.CommitActionOptions{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if commit != nil {
		t.Fatalf("expected nil commit from null body, got %+v", commit)
	}
}

// TestBranchHeadDataSource_NilCommitNoPanic drives the branch_head data source
// (DOS-2) against a server that returns a branch with a null commit, asserting a
// clean error diagnostic instead of a nil-deref panic.
func TestBranchHeadDataSource_NilCommitNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"main","commit":null,"protected":false}`))
	}))
	defer srv.Close()

	client, err := gitlab.NewClient("tok", gitlab.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	d := &branchHeadDataSource{client: client}
	ctx := context.Background()

	schemaResp := &datasource.SchemaResponse{}
	d.Schema(ctx, datasource.SchemaRequest{}, schemaResp)
	sch := schemaResp.Schema

	raw := tftypes.NewValue(sch.Type().TerraformType(ctx), map[string]tftypes.Value{
		"project_id": tftypes.NewValue(tftypes.String, "proj"),
		"branch":     tftypes.NewValue(tftypes.String, "main"),
		"commit_sha": tftypes.NewValue(tftypes.String, nil),
		"protected":  tftypes.NewValue(tftypes.Bool, nil),
	})

	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: raw}}
	resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sch}}

	d.Read(ctx, req, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic for a branch with nil commit, got none")
	}
}

// TestDiffActions_AdoptForwardsLockToken pins CRU-1: when adopt_existing rewrites
// a new-to-state path that already exists remotely into an update, the probed
// last_commit_id is forwarded under optimistic_lock so the overwrite is still
// guarded against a concurrent writer; with optimistic_lock off, no token is sent.
func TestDiffActions_AdoptForwardsLockToken(t *testing.T) {
	const probedLCID = "abc123probed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		actions, err := res.diffActions(context.Background(), plan(true), emptyState())
		if err != nil {
			t.Fatalf("diffActions: %v", err)
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
		actions, err := res.diffActions(context.Background(), plan(false), emptyState())
		if err != nil {
			t.Fatalf("diffActions: %v", err)
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
		actions, err := res.diffActions(context.Background(), plan(true), emptyState)
		if err != nil {
			t.Fatalf("diffActions: %v", err)
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
		actions, err := res.diffActions(context.Background(), plan(false), emptyState)
		if err != nil {
			t.Fatalf("diffActions: %v", err)
		}
		if len(actions) != 1 {
			t.Fatalf("want a single update, got %d actions", len(actions))
		}
	})

	t.Run("remote exec true plan false adds chmod false", func(t *testing.T) {
		remoteExec = true
		defer func() { remoteExec = false }()
		actions, err := res.diffActions(context.Background(), plan(false), emptyState)
		if err != nil {
			t.Fatalf("diffActions: %v", err)
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
