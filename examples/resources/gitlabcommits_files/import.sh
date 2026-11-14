#!/usr/bin/env bash
# The ID is <project_id>::<branch>, with project_id exactly as in the configuration.
# With adopt_existing = true (default), the next apply adopts matching files without a commit.
terraform import 'gitlabcommits_files.service' 'platform/gitops::main'
