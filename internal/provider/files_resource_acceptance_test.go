// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// Acceptance tests require a real GitLab project. Set the following environment
// variables to run them:
//
//	TF_ACC=1
//	GITLAB_TOKEN=<token with `api` scope; see README Authentication>
//	GITLAB_TEST_PROJECT_ID=<group/project> (plain path, not URL-encoded, or numeric ID)
//	GITLAB_TEST_BRANCH=<branch>             (defaults to "tf-acc-test"; must pre-exist unless GITLAB_TEST_BRANCH_FROM is set)
//	GITLAB_TEST_BRANCH_FROM=<ref>           (optional; materialise the branch from this ref and delete it after the test)
//	GITLAB_BASE_URL=<https://gitlab.example.com>  (optional)
//
// Each test is responsible for cleaning up the files it creates so the project
// is left in its original state, no matter the outcome.

const accTestPathPrefix = "tf-acc-test/"

// TestAccFiles_basic creates a two-file bundle in one commit, changes one
// file, and asserts that the next apply lands exactly one commit, which
// updates that file and leaves the other one alone.
func TestAccFiles_basic(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	head := newAccHead(t, project, branch)
	a := accTestPathPrefix + "basic/a.yaml"
	b := accTestPathPrefix + "basic/b.yaml"
	var bBlobID, bLastCommitID string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.oneDestroyCommit(),
			accCheckFileGone(project, branch, a),
			accCheckFileGone(project, branch, b),
		),
		Steps: []resource.TestStep{
			{
				Config:           accConfig(project, branch, map[string]string{a: "version: 1\n", b: "version: 1\n"}),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					resource.TestCheckResourceAttr("gitlabcommits_files.test", "files.%", "2"),
					accCheckFileBytes(project, branch, a, []byte("version: 1\n")),
					accCheckFileBytes(project, branch, b, []byte("version: 1\n")),
					accCaptureAttr("files."+b+".blob_id", &bBlobID),
					accCaptureAttr("files."+b+".last_commit_id", &bLastCommitID),
				),
			},
			{
				// The commit touches a.yaml only, so b.yaml keeps what step 1 recorded.
				Config:           accConfig(project, branch, map[string]string{a: "version: 2\n", b: "version: 1\n"}),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileBytes(project, branch, a, []byte("version: 2\n")),
					accCheckFileBytes(project, branch, b, []byte("version: 1\n")),
					resource.TestCheckResourceAttrPair("gitlabcommits_files.test", "files."+a+".last_commit_id",
						"gitlabcommits_files.test", "commit_sha"),
					resource.TestCheckResourceAttrPtr("gitlabcommits_files.test", "files."+b+".blob_id", &bBlobID),
					resource.TestCheckResourceAttrPtr("gitlabcommits_files.test", "files."+b+".last_commit_id", &bLastCommitID),
				),
			},
		},
	})
}

// TestAccFiles_driftRestored: with detect_drift = true the refresh before
// the plan records a file changed out of band, the plan reverts it, and the
// apply restores the configured content in one commit. That commit carries
// the last_commit_id the refresh recorded, so the optimistic lock lets it
// through.
func TestAccFiles_driftRestored(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	head := newAccHead(t, project, branch)
	path := accTestPathPrefix + "drift/f.txt"

	var b strings.Builder
	b.WriteString(accResourceHeader(project, branch))
	b.WriteString("  detect_drift    = true\n  optimistic_lock = true\n  files = {\n")
	fmt.Fprintf(&b, "    %q = { content = %q }\n", path, "managed\n")
	b.WriteString("  }\n}\n")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.oneDestroyCommit(),
			accCheckFileGone(project, branch, path),
		),
		Steps: []resource.TestStep{
			{
				Config:           b.String(),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check:            head.oneCommit(),
			},
			{
				PreConfig: func() {
					accUpdateFile(t, project, branch, path, "external edit\n")
					head.record(t)
				},
				// The configuration is unchanged, so only the refresh can
				// make this plan an update.
				Config:           b.String(),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileBytes(project, branch, path, []byte("managed\n")),
					resource.TestCheckResourceAttrPair("gitlabcommits_files.test", "files."+path+".last_commit_id",
						"gitlabcommits_files.test", "commit_sha"),
				),
			},
		},
	})
}

// TestAccFiles_addAndRemove proves that adding and removing entries in the
// files map lands one commit per apply, which creates the added file,
// deletes the removed one and leaves the kept one as it was.
func TestAccFiles_addAndRemove(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	head := newAccHead(t, project, branch)
	keep := accTestPathPrefix + "addrm/keep.yaml"
	remove := accTestPathPrefix + "addrm/remove.yaml"
	add := accTestPathPrefix + "addrm/add.yaml"
	var keepLastCommitID string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.oneDestroyCommit(),
			accCheckFileGone(project, branch, keep),
			accCheckFileGone(project, branch, add),
		),
		Steps: []resource.TestStep{
			{
				Config:           accConfig(project, branch, map[string]string{keep: "k\n", remove: "r\n"}),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileBytes(project, branch, keep, []byte("k\n")),
					accCheckFileBytes(project, branch, remove, []byte("r\n")),
					accCaptureAttr("files."+keep+".last_commit_id", &keepLastCommitID),
				),
			},
			{
				// remove.yaml dropped, add.yaml added.
				Config:           accConfig(project, branch, map[string]string{keep: "k\n", add: "a\n"}),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileBytes(project, branch, add, []byte("a\n")),
					accCheckFileBytes(project, branch, keep, []byte("k\n")),
					accCheckFileGone(project, branch, remove),
					resource.TestCheckResourceAttr("gitlabcommits_files.test", "files.%", "2"),
					resource.TestCheckResourceAttrPtr("gitlabcommits_files.test", "files."+keep+".last_commit_id", &keepLastCommitID),
				),
			},
		},
	})
}

// TestAccFiles_directoryToFile: a directory holding only managed files turns
// into a file of the same name in one apply, since the commit deletes the
// files before it creates the new one.
func TestAccFiles_directoryToFile(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	head := newAccHead(t, project, branch)
	dir := accTestPathPrefix + "dir2file/conf"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.oneDestroyCommit(),
			accCheckFileGone(project, branch, dir),
			accCheckFileGone(project, branch, dir+"/app.yaml"),
		),
		Steps: []resource.TestStep{
			{
				Config: accConfig(project, branch, map[string]string{
					dir + "/app.yaml": "a\n",
				}),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileBytes(project, branch, dir+"/app.yaml", []byte("a\n")),
				),
			},
			{
				Config: accConfig(project, branch, map[string]string{
					dir: "c\n",
				}),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileBytes(project, branch, dir, []byte("c\n")),
					accCheckFileGone(project, branch, dir+"/app.yaml"),
				),
			},
		},
	})
}

// TestAccFiles_import covers the round-trip: create a file, make Terraform
// forget the resource without destroying it (a removed block with destroy =
// false), import it into the same state, then re-apply the configuration,
// which adopts the identical file without a commit.
func TestAccFiles_import(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	head := newAccHead(t, project, branch)
	path := accTestPathPrefix + "import/a.yaml"
	// Once the forget step has applied, a failing step can leave a state
	// with no resource or an empty files map, whose destroy deletes nothing
	// and leaves the file on a pre-existing branch.
	accCleanupFile(t, project, branch, path)

	cfg := accConfig(project, branch, map[string]string{path: "v: 1\n"})
	forget := `
provider "gitlabcommits" {}

removed {
  from = gitlabcommits_files.test

  lifecycle {
    destroy = false
  }
}
`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			tfversion.SkipBelow(tfversion.Version1_7_0), // removed blocks
		},
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.oneDestroyCommit(),
			accCheckFileGone(project, branch, path),
		),
		Steps: []resource.TestStep{
			{
				Config:           cfg,
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check:            head.oneCommit(),
			},
			{
				Config: forget,
				Check: resource.ComposeAggregateTestCheckFunc(
					head.noCommit(),
					accCheckFileBytes(project, branch, path, []byte("v: 1\n")),
				),
			},
			{
				// The import step needs its own Config: the previous one
				// declares no resource to import into.
				Config:             cfg,
				ResourceName:       "gitlabcommits_files.test",
				ImportState:        true,
				ImportStateId:      fmt.Sprintf("%s::%s", project, branch),
				ImportStateVerify:  false, // files map is intentionally empty after import
				ImportStatePersist: true,  // the next step must start from the imported, empty-files state
			},
			// Re-apply the same config after import. The plan adds the file
			// to the empty files map, adopt_existing compares the repository
			// content with the plan and finds it identical, so the apply
			// converges without a commit (commit_sha stays unset); the
			// framework's automatic plan-after-apply check enforces zero
			// drift.
			{
				Config:           cfg,
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.noCommit(),
					resource.TestCheckNoResourceAttr("gitlabcommits_files.test", "commit_sha"),
					resource.TestCheckResourceAttr("gitlabcommits_files.test", "files.%", "1"),
					accCheckFileBytes(project, branch, path, []byte("v: 1\n")),
				),
			},
		},
	})
}

// TestAccFiles_binaryContentBase64 round-trips bytes that are not valid
// UTF-8 through content_base64 and asserts the exact bytes land in the
// repository, then re-applies to prove a no-op plan that commits nothing.
func TestAccFiles_binaryContentBase64(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	head := newAccHead(t, project, branch)
	path := accTestPathPrefix + "binary/blob.bin"
	raw := []byte{0x00, 0xff, 0xfe, 0x01, 0x80, 0x7f}

	var b strings.Builder
	b.WriteString(accResourceHeader(project, branch))
	fmt.Fprintf(&b, "  files = {\n    %q = { content_base64 = %q }\n  }\n}\n", path, base64.StdEncoding.EncodeToString(raw))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.oneDestroyCommit(),
			accCheckFileGone(project, branch, path),
		),
		Steps: []resource.TestStep{
			{
				Config:           b.String(),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileBytes(project, branch, path, raw),
				),
			},
			{
				Config:           b.String(),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionNoop),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.noCommit(),
					accCheckFileBytes(project, branch, path, raw),
				),
			},
		},
	})
}

// accCheckFileBytes asserts the exact bytes of path at branch HEAD.
func accCheckFileBytes(project, branch, path string, want []byte) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		c, err := accClient()
		if err != nil {
			return err
		}
		file, _, err := c.RepositoryFiles.GetFile(project, path, &gitlab.GetFileOptions{
			Ref: new(branch),
		}, gitlab.WithContext(context.Background()))
		if err != nil {
			return fmt.Errorf("reading %q on %q: %w", path, branch, err)
		}
		got, err := decodeRemoteContent(file)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("bytes of %q = %x, want %x", path, got, want)
		}
		return nil
	}
}

// --- helpers ---

// accHead follows the head of the test branch through a test case, so each
// step can assert how many commits its apply landed. Every check advances
// it to the head it read; a step that commits out of band calls record
// after doing so.
type accHead struct {
	project, branch string
	sha             string
}

// newAccHead records the commit the first apply of a test starts from.
func newAccHead(t *testing.T, project, branch string) *accHead {
	t.Helper()
	h := &accHead{project: project, branch: branch}
	h.record(t)
	return h
}

// record reads the commit the next apply starts from: the branch head or,
// while the branch does not exist and GITLAB_TEST_BRANCH_FROM is set, the
// commit the provider creates the branch from (that commit SHA, or the head
// of that branch).
func (h *accHead) record(t *testing.T) {
	t.Helper()
	sha, err := accBranchHead(h.project, h.branch)
	if from := accBranchFrom(); errors.Is(err, gitlab.ErrNotFound) && from != "" {
		if isCommitSHA(from) {
			sha, err = strings.ToLower(from), nil
		} else {
			sha, err = accBranchHead(h.project, from)
		}
	}
	if err != nil {
		t.Fatalf("recording the head the next apply starts from: %v", err)
	}
	h.sha = sha
}

// oneCommit asserts that the apply landed exactly one commit, and that it is
// the resource's commit_sha.
func (h *accHead) oneCommit() resource.TestCheckFunc {
	return func(s *terraform.State) error {
		head, err := h.advanceOne()
		if err != nil {
			return err
		}
		rs, ok := s.RootModule().Resources["gitlabcommits_files.test"]
		if !ok || rs.Primary == nil {
			return errors.New("gitlabcommits_files.test is not in state")
		}
		if got := rs.Primary.Attributes["commit_sha"]; got != head {
			return fmt.Errorf("commit_sha = %q, but branch %q is at %s", got, h.branch, head)
		}
		return nil
	}
}

// oneDestroyCommit asserts that terraform destroy landed exactly one commit;
// the resource is still in the state CheckDestroy receives, so commit_sha is
// not compared.
func (h *accHead) oneDestroyCommit() resource.TestCheckFunc {
	return func(*terraform.State) error {
		_, err := h.advanceOne()
		return err
	}
}

// noCommit asserts that the apply, or the destroy, landed no commit.
func (h *accHead) noCommit() resource.TestCheckFunc {
	return func(*terraform.State) error {
		before := h.sha
		head, err := accBranchHead(h.project, h.branch)
		if err != nil {
			return err
		}
		h.sha = head
		if head != before {
			return fmt.Errorf("branch %q moved from %s to %s, but the apply should have landed no commit", h.branch, before, head)
		}
		return nil
	}
}

// advanceOne reads the branch head, advances h to it, and fails unless it is
// exactly one commit on top of the head recorded before.
func (h *accHead) advanceOne() (string, error) {
	before := h.sha
	head, err := accBranchHead(h.project, h.branch)
	if err != nil {
		return "", err
	}
	h.sha = head
	if head == before {
		return "", fmt.Errorf("branch %q is still at %s: the apply landed no commit", h.branch, head)
	}
	c, err := accClient()
	if err != nil {
		return "", err
	}
	commit, _, err := c.Commits.GetCommit(h.project, head, nil, gitlab.WithContext(context.Background()))
	if err != nil {
		return "", fmt.Errorf("reading commit %s: %w", head, err)
	}
	if commit == nil {
		return "", fmt.Errorf("reading commit %s: GitLab returned no commit", head)
	}
	if len(commit.ParentIDs) != 1 || commit.ParentIDs[0] != before {
		return "", fmt.Errorf("branch %q moved from %s to %s, whose parents are %v: the apply did not land exactly one commit",
			h.branch, before, head, commit.ParentIDs)
	}
	return head, nil
}

// accBranchHead returns the id of the head commit of branch.
func accBranchHead(project, branch string) (string, error) {
	c, err := accClient()
	if err != nil {
		return "", err
	}
	b, _, err := c.Branches.GetBranch(project, branch, gitlab.WithContext(context.Background()))
	if err != nil {
		return "", fmt.Errorf("reading branch %q: %w", branch, err)
	}
	if b == nil || b.Commit == nil || b.Commit.ID == "" {
		return "", fmt.Errorf("branch %q has no head commit", branch)
	}
	return b.Commit.ID, nil
}

// accExpectAction asserts the action the plan takes on the test resource
// before it is applied: an update planned as a replacement would land two
// commits, a delete and a create.
func accExpectAction(action plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction("gitlabcommits_files.test", action),
	}}
}

// accCaptureAttr stores a non-empty attribute of the test resource in dst,
// for a later step to compare with TestCheckResourceAttrPtr.
func accCaptureAttr(key string, dst *string) resource.TestCheckFunc {
	return resource.TestCheckResourceAttrWith("gitlabcommits_files.test", key, func(v string) error {
		if v == "" {
			return fmt.Errorf("%s is empty", key)
		}
		*dst = v
		return nil
	})
}

// accUpdateFile commits new content to path out of band.
func accUpdateFile(t *testing.T, project, branch, path, content string) {
	t.Helper()
	c, err := accClient()
	if err != nil {
		t.Fatalf("accClient: %v", err)
	}
	_, _, err = c.RepositoryFiles.UpdateFile(project, path, &gitlab.UpdateFileOptions{
		Branch:        new(branch),
		Content:       new(content),
		CommitMessage: new("tf-acc-test external edit"),
	}, gitlab.WithContext(context.Background()))
	if err != nil {
		t.Fatalf("out-of-band update: %v", err)
	}
}

// accDeleteFile deletes path out of band.
func accDeleteFile(t *testing.T, project, branch, path string) {
	t.Helper()
	c, err := accClient()
	if err != nil {
		t.Fatalf("accClient: %v", err)
	}
	_, err = c.RepositoryFiles.DeleteFile(project, path, &gitlab.DeleteFileOptions{
		Branch:        new(branch),
		CommitMessage: new("tf-acc-test out-of-band delete"),
	}, gitlab.WithContext(context.Background()))
	if err != nil {
		t.Fatalf("out-of-band delete: %v", err)
	}
}

// accCleanupFile deletes path after the test, best-effort, for a test whose
// destroy may leave it behind, so a pre-existing shared branch is left clean
// (per-run branches are deleted wholesale anyway). It runs after
// CheckDestroy, so its commit never reaches the commit checks there.
func accCleanupFile(t *testing.T, project, branch, path string) {
	t.Helper()
	t.Cleanup(func() {
		c, err := accClient()
		if err != nil {
			return
		}
		_, _ = c.RepositoryFiles.DeleteFile(project, path, &gitlab.DeleteFileOptions{
			Branch:        new(branch),
			CommitMessage: new("tf-acc-test cleanup"),
		}, gitlab.WithContext(context.Background()))
	})
}

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

// accCheckFileGone asserts that path is absent at branch HEAD. A 404 counts
// as gone only once a branch lookup succeeds too, since GitLab also answers
// the Files API with 404 when Gitaly fails to resolve the ref (the rule
// absentPaths follows); any other failure is an error, never "gone".
func accCheckFileGone(project, branch, path string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		c, err := accClient()
		if err != nil {
			return err
		}
		_, _, err = c.RepositoryFiles.GetFileMetaData(project, path, &gitlab.GetFileMetaDataOptions{
			Ref: new(branch),
		}, gitlab.WithContext(context.Background()))
		switch {
		case err == nil:
			return fmt.Errorf("expected file %q to be gone on %q, but it exists", path, branch)
		case !errors.Is(err, gitlab.ErrNotFound):
			return fmt.Errorf("checking that %q is gone on %q: %w", path, branch, err)
		}
		if _, err := accBranchHead(project, branch); err != nil {
			return fmt.Errorf("%q answered 404 on %q, but the branch lookup failed: %w", path, branch, err)
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
			Ref: new(branch),
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
	head := newAccHead(t, project, branch)
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
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.oneDestroyCommit(),
			accCheckFileGone(project, branch, path),
		),
		Steps: []resource.TestStep{
			{
				Config:           cfg("v1\n", true),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check:            head.oneCommit(),
			},
			{
				PreConfig: func() {
					accUpdateFile(t, project, branch, path, "external edit\n")
					head.record(t)
				},
				Config:           cfg("v2\n", true),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionUpdate),
				ExpectError:      regexp.MustCompile(`Concurrent modification detected`),
			},
			// Recovery: the rejected apply must land nothing (PreConfig checks
			// that) and leave the prior state, v1 with the stale token,
			// untouched. detect_drift=false blocks a refresh from helping, so
			// re-applying with the lock would 400 forever. Disabling the lock
			// lets the update land in one commit, and only because state still
			// holds v1: a failed apply that had saved v2 would plan no change
			// to the file, commit nothing, and leave the external edit next
			// to a state claiming v2.
			{
				PreConfig: func() {
					rejected := resource.ComposeAggregateTestCheckFunc(
						head.noCommit(),
						accCheckFileBytes(project, branch, path, []byte("external edit\n")),
					)
					if err := rejected(nil); err != nil {
						t.Fatalf("after the rejected apply: %v", err)
					}
				},
				Config:           cfg("v2\n", false),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileBytes(project, branch, path, []byte("v2\n")),
				),
			},
		},
	})
}

// TestAccFiles_destroyAfterOutOfBandDelete: with detect_drift=false state
// still lists a file someone deleted by hand, so the destroy commit names it
// with its last_commit_id and GitLab rejects that commit. Destroy must then
// retry once without the missing file and remove the rest, in one commit.
func TestAccFiles_destroyAfterOutOfBandDelete(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	head := newAccHead(t, project, branch)
	keep := accTestPathPrefix + "oob/keep.txt"
	gone := accTestPathPrefix + "oob/gone.txt"

	var b strings.Builder
	b.WriteString(accResourceHeader(project, branch))
	b.WriteString("  detect_drift = false\n  files = {\n")
	fmt.Fprintf(&b, "    %q = { content = %q }\n", keep, "keep\n")
	fmt.Fprintf(&b, "    %q = { content = %q }\n", gone, "gone\n")
	b.WriteString("  }\n}\n")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.oneDestroyCommit(),
			accCheckFileGone(project, branch, keep),
			accCheckFileGone(project, branch, gone),
		),
		Steps: []resource.TestStep{
			{
				Config:           b.String(),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check:            head.oneCommit(),
			},
			{
				PreConfig: func() {
					accDeleteFile(t, project, branch, gone)
					head.record(t)
				},
				// Read is a no-op with detect_drift=false, so the plan stays
				// empty and state keeps the deleted file for the destroy.
				Config:           b.String(),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionNoop),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.noCommit(),
					accCheckFileGone(project, branch, gone),
					resource.TestCheckResourceAttrSet("gitlabcommits_files.test", "files."+gone+".last_commit_id"),
				),
			},
		},
	})
}

// TestAccFiles_removeAfterOutOfBandDelete: with detect_drift=false state
// still lists a file someone deleted by hand, and its locked delete would be
// rejected on every apply. Removing it from files, in the same apply that
// turns detect_drift back on, must probe the path, drop the delete and
// commit nothing.
func TestAccFiles_removeAfterOutOfBandDelete(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	head := newAccHead(t, project, branch)
	keep := accTestPathPrefix + "oobrm/keep.txt"
	gone := accTestPathPrefix + "oobrm/gone.txt"

	cfg := func(detectDrift bool, paths ...string) string {
		var b strings.Builder
		b.WriteString(accResourceHeader(project, branch))
		fmt.Fprintf(&b, "  detect_drift = %t\n  files = {\n", detectDrift)
		for _, p := range paths {
			fmt.Fprintf(&b, "    %q = { content = %q }\n", p, "content\n")
		}
		b.WriteString("  }\n}\n")
		return b.String()
	}

	var commitSHA string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.oneDestroyCommit(),
			accCheckFileGone(project, branch, keep),
		),
		Steps: []resource.TestStep{
			{
				Config:           cfg(false, keep, gone),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCaptureAttr("commit_sha", &commitSHA),
				),
			},
			{
				PreConfig: func() {
					accDeleteFile(t, project, branch, gone)
					head.record(t)
				},
				// The post-apply plan check fails if state still lists the
				// deleted file.
				Config:           cfg(true, keep),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.noCommit(),
					accCheckFileBytes(project, branch, keep, []byte("content\n")),
					resource.TestCheckResourceAttrPtr("gitlabcommits_files.test", "commit_sha", &commitSHA),
				),
			},
		},
	})
}

// TestAccFiles_chmodCycle flips the executable bit both ways without touching
// content and asserts the bit actually lands in the repository each time.
func TestAccFiles_chmodCycle(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	head := newAccHead(t, project, branch)
	path := accTestPathPrefix + "chmod/tool.sh"
	content := "#!/bin/sh\necho ok\n"

	cfg := func(exec bool) string {
		var b strings.Builder
		b.WriteString(accResourceHeader(project, branch))
		b.WriteString("  files = {\n")
		fmt.Fprintf(&b, "    %q = { content = %q, execute_filemode = %t }\n", path, content, exec)
		b.WriteString("  }\n}\n")
		return b.String()
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.oneDestroyCommit(),
			accCheckFileGone(project, branch, path),
		),
		Steps: []resource.TestStep{
			{
				Config:           cfg(true),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileExec(project, branch, path, true),
				),
			},
			{
				Config:           cfg(false),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileExec(project, branch, path, false),
					accCheckFileBytes(project, branch, path, []byte(content)),
				),
			},
		},
	})
}

// TestAccFiles_keepOnDestroy: delete_on_destroy=false must leave the file in
// the repository after terraform destroy, which then commits nothing.
func TestAccFiles_keepOnDestroy(t *testing.T) {
	testAccPreCheck(t)

	project := os.Getenv("GITLAB_TEST_PROJECT_ID")
	branch := accBranch(t)
	head := newAccHead(t, project, branch)
	path := accTestPathPrefix + "keep/f.txt"

	var b strings.Builder
	b.WriteString(accResourceHeader(project, branch))
	b.WriteString("  delete_on_destroy = false\n  files = {\n")
	fmt.Fprintf(&b, "    %q = { content = %q }\n", path, "keep me\n")
	b.WriteString("  }\n}\n")

	accCleanupFile(t, project, branch, path)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			head.noCommit(),
			accCheckFileBytes(project, branch, path, []byte("keep me\n")),
		),
		Steps: []resource.TestStep{
			{
				Config:           b.String(),
				ConfigPlanChecks: accExpectAction(plancheck.ResourceActionCreate),
				Check: resource.ComposeAggregateTestCheckFunc(
					head.oneCommit(),
					accCheckFileBytes(project, branch, path, []byte("keep me\n")),
				),
			},
		},
	})
}
