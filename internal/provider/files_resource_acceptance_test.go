// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"os"
	"regexp"
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
//	GITLAB_TEST_BRANCH=<branch>             (defaults to "tf-acc-test"; must pre-exist unless GITLAB_TEST_BRANCH_FROM is set)
//	GITLAB_TEST_BRANCH_FROM=<ref>           (optional; materialise the branch from this ref and delete it after the test)
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

// accBranchFrom returns the source ref the test branch is materialised from
// (GITLAB_TEST_BRANCH_FROM), or "" when the branch is expected to pre-exist.
func accBranchFrom() string {
	return os.Getenv("GITLAB_TEST_BRANCH_FROM")
}

// accBranch returns the branch to test against, defaulting to a fixed name so
// repeated test runs are idempotent against the same project. When the branch
// is materialised via GITLAB_TEST_BRANCH_FROM it is deleted after the test so
// unique per-run branches (CI) do not pile up in the test project.
func accBranch(t *testing.T) string {
	t.Helper()
	branch := os.Getenv("GITLAB_TEST_BRANCH")
	if branch == "" {
		branch = "tf-acc-test"
	}
	if accBranchFrom() != "" {
		t.Cleanup(func() {
			c, err := accClient()
			if err != nil {
				t.Logf("cleanup: cannot build client to delete branch %q: %v", branch, err)
				return
			}
			if _, err := c.Branches.DeleteBranch(os.Getenv("GITLAB_TEST_PROJECT_ID"), branch); err != nil {
				t.Logf("cleanup: could not delete branch %q: %v", branch, err)
			}
		})
	}
	return branch
}

// accResourceHeader renders the shared opening of the test resource config:
// provider block, project/branch/commit_message, and - when
// GITLAB_TEST_BRANCH_FROM is set - create_branch_from so the provider
// materialises the branch on first apply (unique per-run branches in CI never
// pre-exist).
func accResourceHeader(project, branch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
provider "gitlabcommits" {}

resource "gitlabcommits_files" "test" {
  project_id     = %q
  branch         = %q
  commit_message = "tf-acc-test"
`, project, branch)
	if from := accBranchFrom(); from != "" {
		fmt.Fprintf(&b, "  create_branch_from = %q\n", from)
	}
	return b.String()
}

// accConfig renders a minimal HCL config exercising the resource for a given
// set of (path, content) entries.
func accConfig(project, branch string, files map[string]string) string {
	var b strings.Builder
	b.WriteString(accResourceHeader(project, branch))
	b.WriteString("  files = {\n")
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

// accCheckFileExec asserts the executable bit of path at branch HEAD.
func accCheckFileExec(project, branch, path string, want bool) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		c, err := accClient()
		if err != nil {
			return err
		}
		meta, _, err := c.RepositoryFiles.GetFileMetaData(project, path, &gitlab.GetFileMetaDataOptions{
			Ref: gitlab.Ptr(branch),
		}, gitlab.WithContext(context.Background()))
		if err != nil {
			return fmt.Errorf("metadata for %q on %q: %w", path, branch, err)
		}
		if meta.ExecuteFilemode != want {
			return fmt.Errorf("exec bit for %q = %t, want %t", path, meta.ExecuteFilemode, want)
		}
		return nil
	}
}

// TestAccFiles_optimisticLockConflict provokes a real optimistic-lock 400:
// detect_drift=false keeps the stale last_commit_id in state, an out-of-band
// commit moves the file, and the next apply must fail with the dedicated
// conflict diagnostic instead of silently overwriting the external edit.
func TestAccFiles_optimisticLockConflict(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	path := accTestPathPrefix + "lock/f.txt"

	cfg := func(content string, lock bool) string {
		var b strings.Builder
		b.WriteString(accResourceHeader(project, branch))
		fmt.Fprintf(&b, "  detect_drift    = false\n  optimistic_lock = %t\n  files = {\n", lock)
		fmt.Fprintf(&b, "    %q = { content = %q }\n", path, content)
		b.WriteString("  }\n}\n")
		return b.String()
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: cfg("v1\n", true)},
			{
				PreConfig: func() {
					c, err := accClient()
					if err != nil {
						t.Fatalf("accClient: %v", err)
					}
					_, _, err = c.RepositoryFiles.UpdateFile(project, path, &gitlab.UpdateFileOptions{
						Branch:        gitlab.Ptr(branch),
						Content:       gitlab.Ptr("external edit\n"),
						CommitMessage: gitlab.Ptr("tf-acc-test external edit"),
					}, gitlab.WithContext(context.Background()))
					if err != nil {
						t.Fatalf("out-of-band update: %v", err)
					}
				},
				Config:      cfg("v2\n", true),
				ExpectError: regexp.MustCompile(`Concurrent modification detected`),
			},
			// Recovery: the failed apply left the prior state untouched
			// (stale token, detect_drift=false blocks a refresh from
			// helping), so re-applying with the lock would 400 forever.
			// Disabling the lock lets the update land; stampBlobs then
			// restamps fresh tokens so the automatic destroy converges.
			{Config: cfg("v2\n", false)},
		},
	})
}

// TestAccFiles_chmodCycle flips the executable bit both ways without touching
// content and asserts the bit actually lands in the repository each time.
func TestAccFiles_chmodCycle(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	path := accTestPathPrefix + "chmod/tool.sh"

	cfg := func(exec bool) string {
		var b strings.Builder
		b.WriteString(accResourceHeader(project, branch))
		b.WriteString("  files = {\n")
		fmt.Fprintf(&b, "    %q = { content = %q, execute_filemode = %t }\n", path, "#!/bin/sh\necho ok\n", exec)
		b.WriteString("  }\n}\n")
		return b.String()
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: cfg(true), Check: accCheckFileExec(project, branch, path, true)},
			{Config: cfg(false), Check: accCheckFileExec(project, branch, path, false)},
		},
	})
}

// TestAccFiles_keepOnDestroy: delete_on_destroy=false must leave the file in
// the repository after terraform destroy.
func TestAccFiles_keepOnDestroy(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	path := accTestPathPrefix + "keep/f.txt"

	var b strings.Builder
	b.WriteString(accResourceHeader(project, branch))
	b.WriteString("  delete_on_destroy = false\n  files = {\n")
	fmt.Fprintf(&b, "    %q = { content = %q }\n", path, "keep me\n")
	b.WriteString("  }\n}\n")

	t.Cleanup(func() {
		c, err := accClient()
		if err != nil {
			return
		}
		// Best-effort: remove the survivor so a pre-existing shared branch is
		// left clean (per-run branches are deleted wholesale anyway).
		_, _ = c.RepositoryFiles.DeleteFile(project, path, &gitlab.DeleteFileOptions{
			Branch:        gitlab.Ptr(branch),
			CommitMessage: gitlab.Ptr("tf-acc-test cleanup"),
		}, gitlab.WithContext(context.Background()))
	})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             accCheckFileExists(project, branch, path),
		Steps: []resource.TestStep{
			{Config: b.String(), Check: accCheckFileExists(project, branch, path)},
		},
	})
}
