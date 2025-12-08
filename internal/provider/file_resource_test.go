package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// TestFileResourceGenerateID tests the ID generation function.
func TestFileResourceGenerateID(t *testing.T) {
	r := &fileResource{}

	tests := []struct {
		name      string
		projectID string
		branch    string
		filePath  string
	}{
		{
			name:      "basic id generation",
			projectID: "my-group/my-project",
			branch:    "main",
			filePath:  "test.txt",
		},
		{
			name:      "special characters",
			projectID: "group/sub-group/project",
			branch:    "feature/my-feature",
			filePath:  "config/app.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id1 := r.generateID(tt.projectID, tt.branch, tt.filePath)
			id2 := r.generateID(tt.projectID, tt.branch, tt.filePath)

			// IDs should be deterministic
			if id1 != id2 {
				t.Errorf("IDs should be deterministic, got different IDs: %s vs %s", id1, id2)
			}

			// IDs should be hex strings of length 64 (SHA256)
			if len(id1) != 64 {
				t.Errorf("Expected ID length of 64, got %d", len(id1))
			}

			// Different inputs should produce different IDs
			id3 := r.generateID(tt.projectID, "different-branch", tt.filePath)
			if id1 == id3 {
				t.Errorf("Different inputs should produce different IDs")
			}
		})
	}
}

// TestFileResourceBuildCommitAction tests the buildCommitAction helper function.
func TestFileResourceBuildCommitAction(t *testing.T) {
	r := &fileResource{}

	tests := []struct {
		name        string
		model       *fileResourceModel
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid create action with content",
			model: &fileResourceModel{
				FilePath: types.StringValue("test.txt"),
				Content:  types.StringValue("test content"),
				Action:   types.StringValue("create"),
			},
			expectError: false,
		},
		{
			name: "default action is update",
			model: &fileResourceModel{
				FilePath: types.StringValue("test.txt"),
				Content:  types.StringValue("test content"),
				Action:   types.StringNull(),
			},
			expectError: false,
		},
		{
			name: "delete action",
			model: &fileResourceModel{
				FilePath: types.StringValue("test.txt"),
				Action:   types.StringValue("delete"),
			},
			expectError: false,
		},
		{
			name: "invalid action",
			model: &fileResourceModel{
				FilePath: types.StringValue("test.txt"),
				Content:  types.StringValue("test"),
				Action:   types.StringValue("invalid"),
			},
			expectError: true,
			errorMsg:    "invalid action",
		},
		{
			name: "both content types specified",
			model: &fileResourceModel{
				FilePath:      types.StringValue("test.txt"),
				Content:       types.StringValue("test"),
				ContentBase64: types.StringValue("dGVzdA=="),
				Action:        types.StringValue("create"),
			},
			expectError: true,
			errorMsg:    "cannot specify both",
		},
		{
			name: "valid base64 content",
			model: &fileResourceModel{
				FilePath:      types.StringValue("test.bin"),
				ContentBase64: types.StringValue("dGVzdCBjb250ZW50"),
				Action:        types.StringValue("create"),
			},
			expectError: false,
		},
		{
			name: "invalid base64",
			model: &fileResourceModel{
				FilePath:      types.StringValue("test.bin"),
				ContentBase64: types.StringValue("not@valid#base64!"),
				Action:        types.StringValue("create"),
			},
			expectError: true,
			errorMsg:    "failed to decode base64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, err := r.buildCommitAction(tt.model)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if tt.errorMsg != "" && !stringContains(err.Error(), tt.errorMsg) {
					t.Errorf("Expected error message to contain '%s', got: %s", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if action == nil {
					t.Errorf("Expected action but got nil")
				}
			}
		})
	}
}

// TestFileResourceActionTypes tests all supported action types.
func TestFileResourceActionTypes(t *testing.T) {
	r := &fileResource{}

	actionTests := map[string]gitlab.FileActionValue{
		"create": gitlab.FileCreate,
		"update": gitlab.FileUpdate,
		"delete": gitlab.FileDelete,
		"move":   gitlab.FileMove,
		"chmod":  gitlab.FileChmod,
	}

	for actionStr, expectedAction := range actionTests {
		t.Run(actionStr, func(t *testing.T) {
			model := &fileResourceModel{
				FilePath: types.StringValue("test.txt"),
				Content:  types.StringValue("test"),
				Action:   types.StringValue(actionStr),
			}

			action, err := r.buildCommitAction(model)
			if err != nil {
				t.Fatalf("Unexpected error for action '%s': %v", actionStr, err)
			}

			if *action.Action != expectedAction {
				t.Errorf("Expected action %v, got %v", expectedAction, *action.Action)
			}
		})
	}
}

// TestBatchManagerGetBatchKey tests batch key generation.
func TestBatchManagerGetBatchKey(t *testing.T) {
	m := &CommitBatchManager{}

	tests := []struct {
		projectID string
		branch    string
		expected  string
	}{
		{
			projectID: "my-project",
			branch:    "main",
			expected:  "my-project/main",
		},
		{
			projectID: "group/sub-group/project",
			branch:    "feature/test",
			expected:  "group/sub-group/project/feature/test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			key := m.getBatchKey(tt.projectID, tt.branch)
			if key != tt.expected {
				t.Errorf("Expected key '%s', got '%s'", tt.expected, key)
			}
		})
	}
}

// TestBatchManagerCreateBatch tests batch creation.
func TestBatchManagerCreateBatch(t *testing.T) {
	m := &CommitBatchManager{
		batches: make(map[string]*CommitBatch),
	}

	projectID := "test-project"
	branch := "main"
	commitMessage := "test commit"
	authorEmail := "test@example.com"
	authorName := "Test User"

	batch := m.getOrCreateBatch(projectID, branch, commitMessage, authorEmail, authorName)

	if batch == nil {
		t.Fatal("Expected batch to be created")
	}

	if batch.ProjectID != projectID {
		t.Errorf("Expected project ID '%s', got '%s'", projectID, batch.ProjectID)
	}

	if batch.Branch != branch {
		t.Errorf("Expected branch '%s', got '%s'", branch, batch.Branch)
	}

	if batch.CommitMessage != commitMessage {
		t.Errorf("Expected commit message '%s', got '%s'", commitMessage, batch.CommitMessage)
	}

	// Getting the same batch again should return the same instance
	batch2 := m.getOrCreateBatch(projectID, branch, commitMessage, authorEmail, authorName)
	if batch != batch2 {
		t.Error("Expected to get the same batch instance")
	}
}

// TestBatchManagerAddFileToBatch tests adding files to a batch.
func TestBatchManagerAddFileToBatch(t *testing.T) {
	m := &CommitBatchManager{
		batches: make(map[string]*CommitBatch),
	}

	batch := m.getOrCreateBatch("project", "main", "commit", "", "")

	action1 := &gitlab.CommitActionOptions{
		FilePath: gitlab.Ptr("file1.txt"),
		Content:  gitlab.Ptr("content1"),
		Action:   gitlab.Ptr(gitlab.FileCreate),
	}

	action2 := &gitlab.CommitActionOptions{
		FilePath: gitlab.Ptr("file2.txt"),
		Content:  gitlab.Ptr("content2"),
		Action:   gitlab.Ptr(gitlab.FileCreate),
	}

	m.addFileToBatch(batch, "file1.txt", action1)
	m.addFileToBatch(batch, "file2.txt", action2)

	if len(batch.Files) != 2 {
		t.Errorf("Expected 2 files in batch, got %d", len(batch.Files))
	}

	if batch.Files["file1.txt"] != action1 {
		t.Error("Expected file1.txt to be in batch")
	}

	if batch.Files["file2.txt"] != action2 {
		t.Error("Expected file2.txt to be in batch")
	}
}

// TestBatchManagerClearBatch tests batch cleanup.
func TestBatchManagerClearBatch(t *testing.T) {
	m := &CommitBatchManager{
		batches: make(map[string]*CommitBatch),
	}

	batch := m.getOrCreateBatch("project", "main", "commit", "", "")
	if batch == nil {
		t.Fatal("Failed to create batch")
	}

	key := m.getBatchKey("project", "main")
	if m.batches[key] == nil {
		t.Fatal("Batch should exist")
	}

	m.clearBatch("project", "main")

	if m.batches[key] != nil {
		t.Error("Batch should be cleared")
	}
}
