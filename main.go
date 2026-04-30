// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

// Package main is the entry point for the GitLab Commits Terraform provider.
// It initializes and starts the provider server that communicates with Terraform
// via the Terraform Plugin Protocol (gRPC).
package main

//go:generate env GOFLAGS=-mod=vendor go tool tfplugindocs generate --provider-dir . --provider-name gitlabcommits

import (
	"context"
	"flag"
	"log"

	"github.com/greeddj/terraform-provider-gitlabcommits/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

// version is the provider version string, typically set during build via ldflags.
// Default value is "dev" for development builds.
var (
	version = "dev"
)

// main initializes and starts the Terraform provider server.
// The provider can be run with -debug flag to enable debugger support.
func main() {
	var debug bool

	// Parse command-line flags.
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	// Configure provider server options.
	opts := providerserver.ServeOpts{
		// Address is the provider's registry identifier used by Terraform.
		// Format: registry.terraform.io/namespace/provider-name
		Address: "registry.terraform.io/greeddj/gitlabcommits",
		// Debug enables debugger support when true.
		Debug: debug,
	}

	// Start the provider server. This blocks until the provider exits.
	err := providerserver.Serve(context.Background(), provider.New(version), opts)

	if err != nil {
		log.Fatal(err.Error())
	}
}
