// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// lfsPointerFor renders the Git LFS pointer file git-lfs writes for content.
func lfsPointerFor(content string) string {
	return fmt.Sprintf("%s\noid sha256:%x\nsize %d\n", lfsPointerVersion, sha256.Sum256([]byte(content)), len(content))
}

func TestParseLFSPointer(t *testing.T) {
	valid := lfsPointerFor("hello")
	oid := fmt.Sprintf("%x", sha256.Sum256([]byte("hello")))
	ext := "ext-0-foo sha256:" + oid + "\n"
	withExt := func(exts string) string { return strings.Replace(valid, "\noid", "\n"+exts+"oid", 1) }
	// fitting is how many extension lines a pointer holds within the size
	// bound; one more makes it too long and changes nothing else.
	fitting := (maxLFSPointerSize - len(valid)) / len(ext)
	cases := map[string]struct {
		raw  string
		want bool
	}{
		"as git-lfs writes it":         {valid, true},
		"with an extension line":       {withExt(ext), true},
		"of an empty file":             {lfsPointerFor(""), true},
		"without the final newline":    {strings.TrimSuffix(valid, "\n"), false},
		"with CRLF line ends":          {strings.ReplaceAll(valid, "\n", "\r\n"), false},
		"with another version":         {strings.Replace(valid, "spec/v1", "spec/v2", 1), false},
		"with the version not first":   {"oid sha256:" + oid + "\n" + lfsPointerVersion + "\nsize 5\n", false},
		"with the size before the oid": {lfsPointerVersion + "\nsize 5\noid sha256:" + oid + "\n", false},
		"with an uppercase oid":        {strings.Replace(valid, oid, strings.ToUpper(oid), 1), false},
		"with a short oid":             {strings.Replace(valid, oid, oid[:63], 1), false},
		"with another hash":            {strings.Replace(valid, "sha256:", "sha1:", 1), false},
		"without a size":               {lfsPointerVersion + "\noid sha256:" + oid + "\n", false},
		"without an oid":               {lfsPointerVersion + "\nsize 5\n", false},
		"with two oids":                {withExt("oid sha256:" + oid + "\n"), false},
		"with a signed size":           {strings.Replace(valid, "size 5", "size +5", 1), false},
		"with a zero-padded size":      {strings.Replace(valid, "size 5", "size 05", 1), false},
		"with a negative size":         {strings.Replace(valid, "size 5", "size -5", 1), false},
		"with a size past int64":       {strings.Replace(valid, "size 5", "size 99999999999999999999", 1), false},
		"with an empty line":           {withExt("\n"), false},
		"with a malformed extension":   {withExt("ext-x-foo sha256:" + oid + "\n"), false},
		"followed by content":          {valid + "more text\n", false},
		"longer than a pointer can be": {withExt(strings.Repeat(ext, fitting+1)), false},
		"of ordinary text":             {"hello\n", false},
	}
	if _, ok := parseLFSPointer([]byte(withExt(strings.Repeat(ext, fitting)))); !ok {
		t.Fatal("a pointer with as many extension lines as fit within the size bound must parse")
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ptr, ok := parseLFSPointer([]byte(c.raw))
			if ok != c.want {
				t.Fatalf("parseLFSPointer(%q) = %v, want %v", c.raw, ok, c.want)
			}
			if ok && c.raw == valid && (!ptr.names([]byte("hello")) || ptr.names([]byte("hellO")) || ptr.names([]byte("hello\n"))) {
				t.Errorf("the pointer must name exactly the bytes it was made from: %+v", ptr)
			}
		})
	}
}

// TestAdoptAwareActions_LFSPointer: the Files API returns an LFS-tracked
// file's pointer, not the file. A pointer to the planned bytes is the same
// file (at most a chmod); a pointer to other content cannot be adopted,
// since GitLab would commit the update as a regular blob and the file would
// leave LFS.
func TestAdoptAwareActions_LFSPointer(t *testing.T) {
	pointer := []byte(lfsPointerFor("payload"))
	cases := []struct {
		name     string
		planned  string
		want     []string
		wantErr  bool
		plannedX bool
	}{
		{name: "pointer to the planned bytes", planned: "payload"},
		{name: "pointer to the planned bytes, exec bit differs", planned: "payload", plannedX: true, want: []string{"chmod:f.bin@l"}},
		{name: "the pointer text itself planned", planned: string(pointer)},
		{name: "pointer to other content", planned: "other", wantErr: true},
		{name: "pointer to a prefix of the planned bytes", planned: "payload!", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := fileModel{Content: types.StringValue(c.planned), ExecuteFilemode: types.BoolValue(c.plannedX)}
			probe := remoteProbe{exists: true, lastCommitID: "l", content: pointer, hasContent: true}
			actions, err := adoptAwareActions("f.bin", f, probe, true)
			if c.wantErr {
				if !errors.Is(err, errLFSPointer) || !strings.Contains(err.Error(), "out of Git LFS") {
					t.Fatalf("want the LFS pointer error, got %v / %v", describeActions(actions), err)
				}
				return
			}
			if err != nil || !slices.Equal(describeActions(actions), c.want) {
				t.Fatalf("actions = %v / %v, want %v", describeActions(actions), err, c.want)
			}
		})
	}
}

// TestAdoptLFSPointer_CreateAndImport drives both adoptions, Create and the
// import round-trip through Update, against a branch whose f.txt is an LFS
// pointer. A pointer to the configured bytes adopts without a commit and
// records the pointer's blob_id, so the next refresh finds no drift; one to
// other content fails the file before anything is committed.
func TestAdoptLFSPointer_CreateAndImport(t *testing.T) {
	type adoption struct {
		run  func(t *testing.T, client *gitlab.Client) (filesResourceModel, bool, string)
		name string
	}
	adoptions := []adoption{
		{name: "Create", run: func(t *testing.T, client *gitlab.Client) (filesResourceModel, bool, string) {
			resp := runCreate(t, client, readState("ignored"))
			var out filesResourceModel
			if resp.Diagnostics.HasError() {
				errs := resp.Diagnostics.Errors()
				return out, true, errs[0].Summary() + ": " + errs[0].Detail()
			}
			resp.State.Get(t.Context(), &out)
			return out, false, ""
		}},
		{name: "import round-trip", run: func(t *testing.T, client *gitlab.Client) (filesResourceModel, bool, string) {
			state := readState("ignored")
			state.Files = map[string]fileModel{}
			resp := runUpdate(t, client, readState("ignored"), state)
			var out filesResourceModel
			if resp.Diagnostics.HasError() {
				errs := resp.Diagnostics.Errors()
				return out, true, errs[0].Summary() + ": " + errs[0].Detail()
			}
			resp.State.Get(t.Context(), &out)
			return out, false, ""
		}},
	}
	for _, a := range adoptions {
		t.Run(a.name+", pointer to the configured bytes", func(t *testing.T) {
			var posts atomic.Int32
			var body string
			out, failed, msg := a.run(t, adoptServer(t, lfsPointerFor("old"), &posts, &body))
			if failed {
				t.Fatalf("unexpected error: %s", msg)
			}
			if got := posts.Load(); got != 0 {
				t.Fatalf("commit POSTs = %d, want 0: the pointer names the configured bytes, body: %s", got, body)
			}
			if f := out.Files["f.txt"]; f.Content.ValueString() != "old" || f.BlobID.ValueString() != "remoteblob" ||
				f.LastCommitID.ValueString() != "remote-lcid" {
				t.Errorf("state = %s / %s / %s, want the configured content with the probed ids", f.Content, f.BlobID, f.LastCommitID)
			}
		})
		t.Run(a.name+", pointer to other content", func(t *testing.T) {
			var posts atomic.Int32
			var body string
			_, failed, msg := a.run(t, adoptServer(t, lfsPointerFor("other"), &posts, &body))
			if !failed || !strings.HasPrefix(msg, "File is stored in Git LFS: ") || !strings.Contains(msg, `file "f.txt"`) ||
				!strings.Contains(msg, "Nothing was committed") {
				t.Fatalf("want the LFS error naming the file, got %q", msg)
			}
			if got := posts.Load(); got != 0 {
				t.Errorf("commit POSTs = %d, want 0", got)
			}
		})
	}
}

// lfsState is readState with f.txt holding content as a string, or base64
// encoded when asBase64 is set.
func lfsState(content string, asBase64 bool) filesResourceModel {
	s := readState("oldblob")
	f := s.Files["f.txt"]
	f.Content = types.StringValue(content)
	if asBase64 {
		f.Content = types.StringNull()
		f.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString([]byte(content)))
	}
	s.Files["f.txt"] = f
	return s
}

// TestRead_LFSPointer: a refresh of a drifted blob sees the pointer of an
// LFS-tracked file. A pointer to the content state holds is no drift, in
// either form and also when state has no blob_id (a stamp after the commit
// failed); only the ids move. A pointer to other content is recorded as it
// is, with a warning that an apply refuses to change the file, unless state
// already records that pointer text.
func TestRead_LFSPointer(t *testing.T) {
	cases := []struct {
		name        string
		held        string
		remote      string
		wantContent string
		wantWarning string
		base64      bool
		nullBlob    bool
	}{
		{name: "pointer to the content in state", remote: lfsPointerFor("old"), wantContent: "old"},
		{name: "pointer to the content in state, no blob_id", remote: lfsPointerFor("old"), wantContent: "old", nullBlob: true},
		{name: "pointer to the content_base64 in state", remote: lfsPointerFor("old"), wantContent: "old", base64: true},
		{name: "pointer to other content", remote: lfsPointerFor("new"), wantContent: lfsPointerFor("new"),
			wantWarning: `file "f.txt" on branch "main" holds a Git LFS pointer`},
		{name: "pointer to other content_base64", remote: lfsPointerFor("new"), wantContent: lfsPointerFor("new"), base64: true,
			wantWarning: `file "f.txt" on branch "main" holds a Git LFS pointer`},
		{name: "the pointer text already in state", held: lfsPointerFor("new"), remote: lfsPointerFor("new"),
			wantContent: lfsPointerFor("new")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &repoFake{files: map[string]string{"f.txt": "newlcid"}, content: map[string][]byte{"f.txt": []byte(c.remote)}}
			held := c.held
			if held == "" {
				held = "old"
			}
			state := lfsState(held, c.base64)
			if c.nullBlob {
				f := state.Files["f.txt"]
				f.BlobID = types.StringNull()
				state.Files["f.txt"] = f
			}
			resp, out := runRead(t, fake.client(t), state)
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
			}
			wantDiag(t, "warning", resp.Diagnostics.Warnings(), c.wantWarning)
			f := out.Files["f.txt"]
			got, err := f.rawBytes()
			if err != nil || string(got) != c.wantContent || f.Content.IsNull() != c.base64 {
				t.Errorf("state content = %s / %s, want %q in the configured form", f.Content, f.ContentBase64, c.wantContent)
			}
			if f.BlobID.ValueString() != "blob-newlcid" || f.LastCommitID.ValueString() != "newlcid" {
				t.Errorf("blob_id/last_commit_id = %s/%s, want blob-newlcid/newlcid", f.BlobID, f.LastCommitID)
			}
		})
	}
}

// TestUpdate_LFSPointerInState: once a refresh has recorded an LFS pointer
// in state, the pointer is what the branch holds. A plan of the object it
// names changes nothing and commits nothing; any other content change, and
// a chmod, is refused before the commit, since GitLab would commit the file
// as a regular blob outside LFS.
func TestUpdate_LFSPointerInState(t *testing.T) {
	pointer := lfsPointerFor("new")
	cases := []struct {
		name    string
		planned string
		wantErr bool
		exec    bool
	}{
		{name: "plan names the object", planned: "new"},
		{name: "plan keeps the pointer text", planned: pointer},
		{name: "plan changes the content", planned: "old", wantErr: true},
		{name: "plan names the object and flips the exec bit", planned: "new", exec: true, wantErr: true},
		{name: "plan keeps the pointer text and flips the exec bit", planned: pointer, exec: true, wantErr: true},
	}
	for _, c := range cases {
		for _, asBase64 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, base64 %v", c.name, asBase64), func(t *testing.T) {
				fake := &repoFake{files: map[string]string{"f.txt": "oldlcid"}}
				state := lfsState(pointer, asBase64)
				plan := lfsState(c.planned, asBase64)
				f := plan.Files["f.txt"]
				f.ExecuteFilemode = types.BoolValue(c.exec)
				plan.Files["f.txt"] = f

				resp := runUpdate(t, fake.client(t), plan, state)
				commits, _, _ := fake.recorded()
				if len(commits) != 0 {
					t.Errorf("commits = %q, want none", commits)
				}
				if c.wantErr {
					wantDiag(t, "error", resp.Diagnostics.Errors(), "File is stored in Git LFS")
					if errs := resp.Diagnostics.Errors(); len(errs) == 1 && !strings.Contains(errs[0].Detail(), `file "f.txt"`) {
						t.Errorf("the error must name the file, got: %s", errs[0].Detail())
					}
					return
				}
				if resp.Diagnostics.HasError() {
					t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
				}
				var out filesResourceModel
				resp.State.Get(t.Context(), &out)
				got := out.Files["f.txt"]
				if held, _ := got.rawBytes(); string(held) != c.planned || got.BlobID.ValueString() != "oldblob" ||
					got.LastCommitID.ValueString() != "oldlcid" {
					t.Errorf("state = %q / %s / %s, want the planned content with the ids in state", held, got.BlobID, got.LastCommitID)
				}
			})
		}
	}
}
