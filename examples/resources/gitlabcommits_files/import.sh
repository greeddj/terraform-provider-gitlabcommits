#!/usr/bin/env bash
# Import format: <project_id>::<branch>
#
# After import the files map is empty in state. The next plan reconciles
# the user's HCL with the repository, and adopt_existing=true (default)
# rewrites "create" actions for paths that already exist into "update"s,
# so the apply converges without duplicate-file errors.
terraform import 'gitlabcommits_files.service' 'platform/gitops::main'
