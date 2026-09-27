// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// TestTerraformPlan_ShowsOnlyWhatChanges runs Terraform itself against
// repoFake. A plan that edits one file of three shows only that file's
// computed values and commit_sha as known after apply, and a plan that only
// edits commit_message shows no computed value changing and commits nothing.
// Terraform checks every apply against its plan, so a value the plan kept
// and the apply changed would fail the step as an inconsistent result. It
// needs a terraform CLI (on PATH, or TF_ACC_TERRAFORM_PATH), which CI sets
// up, and is skipped without one rather than downloading it.
func TestTerraformPlan_ShowsOnlyWhatChanges(t *testing.T) {
	if os.Getenv("TF_ACC_TERRAFORM_PATH") == "" {
		if _, err := exec.LookPath("terraform"); err != nil {
			t.Skip("no terraform CLI on PATH and TF_ACC_TERRAFORM_PATH unset")
		}
	}
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
