// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// runFileDataSourceRead drives fileDataSource.Read for the fixed config
// (proj, main, f.bin) against a faked client and returns the response plus
// the resulting model on success.
func runFileDataSourceRead(t *testing.T, client *gitlab.Client) (*datasource.ReadResponse, fileDataSourceModel) {
	t.Helper()
	d := &fileDataSource{client: client}
	ctx := context.Background()

	schemaResp := &datasource.SchemaResponse{}
	d.Schema(ctx, datasource.SchemaRequest{}, schemaResp)
	sch := schemaResp.Schema

	raw := tftypes.NewValue(sch.Type().TerraformType(ctx), map[string]tftypes.Value{
		"project_id":       tftypes.NewValue(tftypes.String, "proj"),
		"branch":           tftypes.NewValue(tftypes.String, "main"),
		"file_path":        tftypes.NewValue(tftypes.String, "f.bin"),
		"content":          tftypes.NewValue(tftypes.String, nil),
		"content_base64":   tftypes.NewValue(tftypes.String, nil),
		"blob_id":          tftypes.NewValue(tftypes.String, nil),
		"last_commit_id":   tftypes.NewValue(tftypes.String, nil),
		"size":             tftypes.NewValue(tftypes.Number, nil),
		"execute_filemode": tftypes.NewValue(tftypes.Bool, nil),
	})
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: raw}}
	resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sch}}
	d.Read(ctx, req, resp)

	var out fileDataSourceModel
	if !resp.Diagnostics.HasError() {
		resp.State.Get(ctx, &out)
	}
	return resp, out
}

// TestFileDataSource_HappyPath: every attribute is populated from the wire
// response, with content decoded and content_base64 round-tripping the raw
// bytes.
func TestFileDataSource_HappyPath(t *testing.T) {
	content := []byte("hello world\n")
	encoded := base64.StdEncoding.EncodeToString(content)
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"file_name":"f.bin","file_path":"f.bin","size":12,"encoding":"base64","content":"` +
			encoded + `","blob_id":"blob123","last_commit_id":"lcid123","execute_filemode":true}`))
	})

	resp, out := runFileDataSourceRead(t, client)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if out.Content.ValueString() != string(content) {
		t.Errorf("content = %q, want %q", out.Content.ValueString(), string(content))
	}
	if out.ContentBase64.ValueString() != encoded {
		t.Errorf("content_base64 = %q, want %q", out.ContentBase64.ValueString(), encoded)
	}
	if out.BlobID.ValueString() != "blob123" {
		t.Errorf("blob_id = %q, want %q", out.BlobID.ValueString(), "blob123")
	}
	if out.LastCommitID.ValueString() != "lcid123" {
		t.Errorf("last_commit_id = %q, want %q", out.LastCommitID.ValueString(), "lcid123")
	}
	if out.Size.ValueInt64() != 12 {
		t.Errorf("size = %d, want 12", out.Size.ValueInt64())
	}
	if !out.ExecuteFilemode.ValueBool() {
		t.Error("execute_filemode must be true")
	}
}

// TestFileDataSource_NotFound: a 404 maps to the dedicated friendly
// diagnostic, not a generic API error.
func TestFileDataSource_NotFound(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})

	resp, _ := runFileDataSourceRead(t, client)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for a missing file")
	}
	if got := resp.Diagnostics.Errors()[0].Summary(); got != "File not found" {
		t.Errorf("summary = %q, want %q", got, "File not found")
	}
}

// TestFileDataSource_BinaryContentNull: invalid UTF-8 must leave content null
// (cty would mangle the bytes) while content_base64 stays authoritative.
func TestFileDataSource_BinaryContentNull(t *testing.T) {
	binary := []byte{0xff, 0xfe, 0x00, 0x01}
	encoded := base64.StdEncoding.EncodeToString(binary)
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"file_path":"f.bin","size":4,"encoding":"base64","content":"` +
			encoded + `","blob_id":"blob123","last_commit_id":"lcid123"}`))
	})

	resp, out := runFileDataSourceRead(t, client)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if !out.Content.IsNull() {
		t.Errorf("content must be null for binary files, got %q", out.Content.ValueString())
	}
	if out.ContentBase64.ValueString() != encoded {
		t.Errorf("content_base64 = %q, want %q", out.ContentBase64.ValueString(), encoded)
	}
}

// TestFileDataSource_DecodeErrorSurfaces: an unknown encoding fails loudly
// instead of passing the wire string through.
func TestFileDataSource_DecodeErrorSurfaces(t *testing.T) {
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"file_path":"f.bin","size":4,"encoding":"rot13","content":"abcd","blob_id":"b","last_commit_id":"l"}`))
	})

	resp, _ := runFileDataSourceRead(t, client)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for an unknown content encoding")
	}
	if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "rot13") {
		t.Errorf("diagnostic should name the unexpected encoding, got: %v", resp.Diagnostics.Errors())
	}
}

// TestFileDataSource_OversizedBlobIDIgnored mirrors the resource's hostile
// blob_id ceiling: an absurd blob_id yields a null attribute plus a warning,
// never a state entry of unbounded size.
func TestFileDataSource_OversizedBlobIDIgnored(t *testing.T) {
	huge := strings.Repeat("a", maxBlobIDLen+1)
	client := newReadClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"file_path":"f.bin","size":2,"encoding":"base64","content":"` +
			base64.StdEncoding.EncodeToString([]byte("ok")) + `","blob_id":"` + huge + `","last_commit_id":"l"}`))
	})

	resp, out := runFileDataSourceRead(t, client)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %v", resp.Diagnostics.Errors())
	}
	if !out.BlobID.IsNull() {
		t.Error("oversized blob_id must be dropped, not stored")
	}
	if resp.Diagnostics.WarningsCount() == 0 {
		t.Error("expected a warning about the oversized blob_id")
	}
}
