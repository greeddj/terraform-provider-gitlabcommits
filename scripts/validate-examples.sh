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
# The resource and data-source snippets are rendered into the Registry docs
# but are not modules; each is validated in a copy with the provider source
# added.
for dir in examples/resources/*/ examples/data-sources/*/; do
  name="$(basename "$dir")"
  mod="$tmp/snippets/$name"
  mkdir -p "$mod"
  cp "$dir"*.tf "$mod/"
  cat > "$mod/zz_required_providers.tf" <<EOF
terraform {
  required_providers {
    gitlabcommits = {
      source = "greeddj/gitlabcommits"
    }
  }
}
EOF
  echo "validating $dir"
  (cd "$mod" && TF_CLI_CONFIG_FILE="$tmp/dev.tfrc" terraform validate)
done
