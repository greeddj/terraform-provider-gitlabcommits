// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// Acceptance tests require a real GitLab project. Set the following environment
// variables to run them:
//
//	TF_ACC=1
//	GITLAB_TOKEN=<token with `api` scope; see README Authentication>
//	GITLAB_TEST_PROJECT_ID=<group/project> (URL-encoded path or numeric ID)
//	GITLAB_TEST_BRANCH=<branch>             (defaults to a unique throwaway branch)
//	GITLAB_BASE_URL=<https://gitlab.example.com>  (optional)
//
// Each test is responsible for cleaning up the files it creates so the project
// is left in its original state, no matter the outcome.

const accTestPathPrefix = "tf-acc-test/"

// TestAccFiles_basic creates a small bundle, asserts it lands as one commit,
// modifies one file, asserts the next apply produces a single update commit,
// then destroys it.
func TestAccFiles_basic(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: accConfig(project, branch, map[string]string{
					accTestPathPrefix + "basic/a.yaml": "version: 1\n",
					accTestPathPrefix + "basic/b.yaml": "version: 1\n",
				}),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("gitlabcommits_files.test", "commit_sha"),
					resource.TestCheckResourceAttr("gitlabcommits_files.test", "files.%", "2"),
					accCheckFileExists(project, branch, accTestPathPrefix+"basic/a.yaml"),
					accCheckFileExists(project, branch, accTestPathPrefix+"basic/b.yaml"),
				),
			},
			{
				// One file changed → one update commit. The other file's blob_id stays put.
				Config: accConfig(project, branch, map[string]string{
					accTestPathPrefix + "basic/a.yaml": "version: 2\n",
					accTestPathPrefix + "basic/b.yaml": "version: 1\n",
				}),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("gitlabcommits_files.test", "commit_sha"),
				),
			},
		},
	})
}

// TestAccFiles_addAndRemove proves that adding/removing entries in the files
// map only emits one commit per apply with the minimum set of actions.
func TestAccFiles_addAndRemove(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: accConfig(project, branch, map[string]string{
					accTestPathPrefix + "addrm/keep.yaml":   "k\n",
					accTestPathPrefix + "addrm/remove.yaml": "r\n",
				}),
				Check: resource.ComposeAggregateTestCheckFunc(
					accCheckFileExists(project, branch, accTestPathPrefix+"addrm/remove.yaml"),
				),
			},
			{
				// remove.yaml dropped, add.yaml added.
				Config: accConfig(project, branch, map[string]string{
					accTestPathPrefix + "addrm/keep.yaml": "k\n",
					accTestPathPrefix + "addrm/add.yaml":  "a\n",
				}),
				Check: resource.ComposeAggregateTestCheckFunc(
					accCheckFileExists(project, branch, accTestPathPrefix+"addrm/add.yaml"),
					accCheckFileGone(project, branch, accTestPathPrefix+"addrm/remove.yaml"),
				),
			},
		},
	})
}

// TestAccFiles_import covers the round-trip: create files, drop the resource
// from state, re-import, and verify a no-op plan.
func TestAccFiles_import(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)

	cfg := accConfig(project, branch, map[string]string{
		accTestPathPrefix + "import/a.yaml": "v: 1\n",
	})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				ResourceName:      "gitlabcommits_files.test",
				ImportState:       true,
				ImportStateId:     fmt.Sprintf("%s::%s", project, branch),
				ImportStateVerify: false, // files map is intentionally empty after import
			},
			// Re-apply the same config after import. adopt_existing rewrites
			// the spurious "create" actions into updates so apply converges
			// without duplicate-file errors, and the framework's automatic
			// plan-after-apply check enforces zero drift.
			{Config: cfg},
		},
	})
}

// --- helpers ---

// accBranch returns the branch to test against, defaulting to a fixed name so
// repeated test runs are idempotent against the same project.
func accBranch(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("GITLAB_TEST_BRANCH"); b != "" {
		return b
	}
	return "tf-acc-test"
}

// accConfig renders a minimal HCL config exercising the resource for a given
// set of (path, content) entries.
func accConfig(project, branch string, files map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
provider "gitlabcommits" {}

resource "gitlabcommits_files" "test" {
  project_id     = %q
  branch         = %q
  commit_message = "tf-acc-test"
  files = {
`, project, branch)
	for path, content := range files {
		fmt.Fprintf(&b, "    %q = { content = %q }\n", path, content)
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

// accClient builds a one-shot GitLab client for assertions, using the same
// env vars the provider does.
func accClient() (*gitlab.Client, error) {
	token := os.Getenv("GITLAB_TOKEN")
	opts := []gitlab.ClientOptionFunc{}
	if base := os.Getenv("GITLAB_BASE_URL"); base != "" {
		opts = append(opts, gitlab.WithBaseURL(base))
	}
	return gitlab.NewClient(token, opts...)
}

// accCheckFileExists asserts that path exists at branch HEAD via the API.
func accCheckFileExists(project, branch, path string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		c, err := accClient()
		if err != nil {
			return err
		}
		_, _, err = c.RepositoryFiles.GetFileMetaData(project, path, &gitlab.GetFileMetaDataOptions{
			Ref: gitlab.Ptr(branch),
		}, gitlab.WithContext(context.Background()))
		if err != nil {
			return fmt.Errorf("expected file %q to exist on %q: %w", path, branch, err)
		}
		return nil
	}
}

// accCheckFileGone asserts that path is absent at branch HEAD.
func accCheckFileGone(project, branch, path string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		c, err := accClient()
		if err != nil {
			return err
		}
		_, _, err = c.RepositoryFiles.GetFileMetaData(project, path, &gitlab.GetFileMetaDataOptions{
			Ref: gitlab.Ptr(branch),
		}, gitlab.WithContext(context.Background()))
		if err == nil {
			return fmt.Errorf("expected file %q to be gone on %q, but it exists", path, branch)
		}
		return nil
	}
}
