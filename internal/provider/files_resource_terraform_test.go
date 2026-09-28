// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// requireTerraform skips a test that runs Terraform itself when there is no
// terraform CLI (on PATH, or TF_ACC_TERRAFORM_PATH), which CI sets up,
// rather than downloading one.
func requireTerraform(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC_TERRAFORM_PATH") == "" {
		if _, err := exec.LookPath("terraform"); err != nil {
			t.Skip("no terraform CLI on PATH and TF_ACC_TERRAFORM_PATH unset")
		}
	}
}

// TestTerraformPlan_ShowsOnlyWhatChanges runs Terraform itself against
// repoFake. A plan that edits one file of three shows only that file's
// computed values and commit_sha as known after apply, and a plan that only
// edits commit_message shows no computed value changing and commits nothing.
// Terraform checks every apply against its plan, so a value the plan kept
// and the apply changed would fail the step as an inconsistent result.
func TestTerraformPlan_ShowsOnlyWhatChanges(t *testing.T) {
	requireTerraform(t)
	fake := &repoFake{files: map[string]string{}}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)

	config := func(message, aContent string) string {
		return fmt.Sprintf(`
provider "gitlabcommits" {
  token       = "tok"
  base_url    = %q
  max_retries = 0
}

resource "gitlabcommits_files" "test" {
  project_id     = "proj"
  branch         = "main"
  commit_message = %q
  files = {
    "d/a.yaml" = { content = %q }
    "d/b.yaml" = { content = "b" }
    "d/c.yaml" = { content = "c" }
  }
}
`, srv.URL, message, aContent)
	}
	const addr = "gitlabcommits_files.test"
	file := func(p, name string) tfjsonpath.Path {
		return tfjsonpath.New("files").AtMapKey(p).AtMapKey(name)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config("one", "a")},
			{
				Config: config("one", "a2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
					plancheck.ExpectUnknownValue(addr, file("d/a.yaml", "blob_id")),
					plancheck.ExpectUnknownValue(addr, file("d/a.yaml", "last_commit_id")),
					plancheck.ExpectKnownValue(addr, file("d/b.yaml", "blob_id"), knownvalue.StringExact("blob-sha1")),
					plancheck.ExpectKnownValue(addr, file("d/b.yaml", "last_commit_id"), knownvalue.StringExact("sha1")),
					plancheck.ExpectKnownValue(addr, file("d/c.yaml", "blob_id"), knownvalue.StringExact("blob-sha1")),
					plancheck.ExpectKnownValue(addr, file("d/c.yaml", "last_commit_id"), knownvalue.StringExact("sha1")),
					plancheck.ExpectUnknownValue(addr, tfjsonpath.New("commit_sha")),
				}},
			},
			{
				Config: config("two", "a2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue(addr, file("d/a.yaml", "blob_id"), knownvalue.StringExact("blob-sha2")),
					plancheck.ExpectKnownValue(addr, file("d/a.yaml", "last_commit_id"), knownvalue.StringExact("sha2")),
					plancheck.ExpectKnownValue(addr, file("d/b.yaml", "blob_id"), knownvalue.StringExact("blob-sha1")),
					plancheck.ExpectKnownValue(addr, tfjsonpath.New("commit_sha"), knownvalue.StringExact("sha2")),
				}},
			},
		},
	})

	commits, _, _ := fake.recorded()
	want := [][]string{
		{"create:d/a.yaml", "create:d/b.yaml", "create:d/c.yaml"},
		{"update:d/a.yaml@sha1"},
		{"delete:d/a.yaml@sha2", "delete:d/b.yaml@sha1", "delete:d/c.yaml@sha1"},
	}
	if !slices.EqualFunc(commits, want, slices.Equal) {
		t.Errorf("commits = %q, want %q", commits, want)
	}
}

// TestTerraformRefresh_NonNFCDriftIsPlanned runs Terraform itself against
// repoFake: a file managed through content is rewritten out of band to the
// same text in another Unicode form, a combining accent in place of the
// precomposed letter. Terraform normalises every string a provider returns
// to NFC, so recorded in content that drift would turn back into the
// configured text next to the new blob_id, and no plan would ever show it.
// Recorded in content_base64 it is planned, and the apply writes the
// configured NFC bytes back in one commit.
func TestTerraformRefresh_NonNFCDriftIsPlanned(t *testing.T) {
	requireTerraform(t)
	const nfc, nfd = "caf\u00e9\n", "cafe\u0301\n"
	fake := &repoFake{files: map[string]string{}, content: map[string][]byte{}}
	var mu sync.Mutex
	var written []string
	serve := fake.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			var opts gitlab.CreateCommitOptions
			if err := json.Unmarshal(body, &opts); err == nil {
				mu.Lock()
				for _, a := range opts.Actions {
					if a.Content != nil {
						written = append(written, *a.Content)
					}
				}
				mu.Unlock()
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		serve(w, r)
	}))
	t.Cleanup(srv.Close)

	config := fmt.Sprintf(`
provider "gitlabcommits" {
  token       = "tok"
  base_url    = %q
  max_retries = 0
}

resource "gitlabcommits_files" "test" {
  project_id     = "proj"
  branch         = "main"
  commit_message = "sync"
  files = {
    "notes.txt" = { content = "caf\u00e9\n" }
  }
}
`, srv.URL)
	const addr = "gitlabcommits_files.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					fake.mu.Lock()
					defer fake.mu.Unlock()
					fake.files["notes.txt"] = "oob"
					fake.content["notes.txt"] = []byte(nfd)
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
				}},
			},
		},
	})

	commits, _, _ := fake.recorded()
	want := [][]string{
		{"create:notes.txt"},
		{"update:notes.txt@oob"},
		{"delete:notes.txt@sha2"},
	}
	if !slices.EqualFunc(commits, want, slices.Equal) {
		t.Errorf("commits = %q, want %q", commits, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(written, []string{nfc, nfc}) {
		t.Errorf("content written = %q, want the configured NFC text on create and on the update", written)
	}
}
