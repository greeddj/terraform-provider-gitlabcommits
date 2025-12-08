package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// TestCommitResourceBuildCommitActions tests the buildCommitActions helper function.
func TestCommitResourceBuildCommitActions(t *testing.T) {
	r := &commitResource{}

	tests := []struct {
		name        string
		files       []fileModel
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid create action",
			files: []fileModel{
				{
					FilePath: types.StringValue("test.txt"),
					Content:  types.StringValue("test content"),
					Action:   types.StringValue("create"),
				},
			},
			expectError: false,
		},
		{
			name: "valid update action with default",
			files: []fileModel{
				{
					FilePath: types.StringValue("test.txt"),
					Content:  types.StringValue("test content"),
					Action:   types.StringNull(),
				},
			},
			expectError: false,
		},
		{
			name: "invalid action",
			files: []fileModel{
				{
					FilePath: types.StringValue("test.txt"),
					Content:  types.StringValue("test content"),
					Action:   types.StringValue("invalid"),
				},
			},
			expectError: true,
			errorMsg:    "invalid action",
		},
		{
			name: "both content and content_base64 specified",
			files: []fileModel{
				{
					FilePath:      types.StringValue("test.txt"),
					Content:       types.StringValue("test"),
					ContentBase64: types.StringValue("dGVzdA=="),
					Action:        types.StringValue("create"),
				},
			},
			expectError: true,
			errorMsg:    "cannot specify both",
		},
		{
			name: "valid base64 content",
			files: []fileModel{
				{
					FilePath:      types.StringValue("test.bin"),
					ContentBase64: types.StringValue("dGVzdA=="),
					Action:        types.StringValue("create"),
				},
			},
			expectError: false,
		},
		{
			name: "invalid base64 content",
			files: []fileModel{
				{
					FilePath:      types.StringValue("test.bin"),
					ContentBase64: types.StringValue("not-valid-base64!@#"),
					Action:        types.StringValue("create"),
				},
			},
			expectError: true,
			errorMsg:    "failed to decode base64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions, err := r.buildCommitActions(tt.files)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if tt.errorMsg != "" && !contains(err.Error(), tt.errorMsg) {
					t.Errorf("Expected error message to contain '%s', got: %s", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if len(actions) != len(tt.files) {
					t.Errorf("Expected %d actions, got %d", len(tt.files), len(actions))
				}
			}
		})
	}
}

// TestCommitResourceActionTypes tests all supported action types.
func TestCommitResourceActionTypes(t *testing.T) {
	r := &commitResource{}

	actionTests := map[string]gitlab.FileActionValue{
		"create": gitlab.FileCreate,
		"update": gitlab.FileUpdate,
		"delete": gitlab.FileDelete,
		"move":   gitlab.FileMove,
		"chmod":  gitlab.FileChmod,
	}

	for actionStr, expectedAction := range actionTests {
		t.Run(actionStr, func(t *testing.T) {
			files := []fileModel{
				{
					FilePath: types.StringValue("test.txt"),
					Content:  types.StringValue("test"),
					Action:   types.StringValue(actionStr),
				},
			}

			actions, err := r.buildCommitActions(files)
			if err != nil {
				t.Fatalf("Unexpected error for action '%s': %v", actionStr, err)
			}

			if len(actions) != 1 {
				t.Fatalf("Expected 1 action, got %d", len(actions))
			}

			if *actions[0].Action != expectedAction {
				t.Errorf("Expected action %v, got %v", expectedAction, *actions[0].Action)
			}
		})
	}
}

// contains checks if a string contains a substring.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
