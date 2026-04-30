// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// testAccProtoV6ProviderFactories instantiates the provider for acceptance
// tests. Acceptance tests require a real GitLab instance reachable through the
// GITLAB_TOKEN / GITLAB_BASE_URL / GITLAB_TEST_PROJECT_ID environment variables.
//
//lint:ignore U1000 referenced by future acceptance tests
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"gitlabcommits": providerserver.NewProtocol6WithError(New("test")()),
}

// testAccPreCheck validates that required environment variables are set before
// running acceptance tests. Skips tests gracefully when run without a configured
// GitLab project so the unit-test target stays green.
//
//lint:ignore U1000 referenced by future acceptance tests
func testAccPreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set, skipping acceptance test")
	}
	if os.Getenv("GITLAB_TOKEN") == "" {
		t.Fatal("GITLAB_TOKEN must be set for acceptance tests")
	}
	if os.Getenv("GITLAB_TEST_PROJECT_ID") == "" {
		t.Fatal("GITLAB_TEST_PROJECT_ID must be set for acceptance tests")
	}
}
