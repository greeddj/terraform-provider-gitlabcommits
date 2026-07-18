#!/bin/sh
# Copyright Dmitrij Shishkin (greeddj@gmail.com) 2025, 2026
# SPDX-License-Identifier: MIT

# Validates every example module against the locally built provider via
# dev_overrides, so terraform validate needs neither terraform init nor a
# published release. Shared by `just check-examples` and CI.
set -eu
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
go build -o "$tmp/terraform-provider-gitlabcommits" .
cat > "$tmp/dev.tfrc" <<EOF
provider_installation {
  dev_overrides {
    "greeddj/gitlabcommits" = "$tmp"
  }
  direct {}
}
EOF
for dir in examples/complete examples/for_each examples/provider; do
  echo "validating $dir"
  (cd "$dir" && TF_CLI_CONFIG_FILE="$tmp/dev.tfrc" terraform validate)
done
