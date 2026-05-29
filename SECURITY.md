# Security policy

## Reporting a vulnerability

If you believe you've found a security issue in this provider, **do not open
a public GitHub issue**. Use one of these private channels instead:

- **Preferred:** open a GitHub Security Advisory at
  <https://github.com/greeddj/terraform-provider-gitlabcommits/security/advisories/new>.
  This keeps the discussion threaded against a CVE-trackable record and lets
  us collaborate on a fix in a private fork.
- **Alternative:** email the maintainer directly at <greeddj@gmail.com>.

Please include:
- a description of the issue and the impact you observed,
- a minimal reproduction (HCL config + steps),
- the provider version (`terraform providers` output) and target GitLab version.

You can expect an initial reply within 7 days. If we confirm the issue we'll
agree on a coordinated disclosure timeline before any public fix lands.

## Scope

In scope:
- the provider itself (this repository) and its release artifacts,
- the GitLab API interactions performed by the provider.

Out of scope:
- vulnerabilities in GitLab itself — please report those at
  https://about.gitlab.com/security,
- vulnerabilities in Terraform Core or HashiCorp's Plugin Framework.

## Threat model

This provider is designed for managing **non-secret** configuration files in
GitLab repositories - ArgoCD `Application` manifests, Helm `values.yaml`,
GitLab CI declarations, Kubernetes manifests, etc.

### Out of scope: secret material in `content`

The `content` and `content_base64` attributes are deliberately **not marked
`Sensitive`**. Values are visible in `terraform plan` / `terraform apply` output
(including CI logs) and in `terraform.tfstate`. This is by design - config files
are reviewed in code review, and a redacted plan defeats that.

If you need to deliver a secret to a workload, do not put it in a file managed
by this provider. Use SealedSecrets, the ExternalSecrets Operator, Vault Agent /
CSI Secrets Store, or masked GitLab CI variables, and reference the secret
resource by name from the managed file.

## What we won't fix

- Token exposure caused by the user's HCL or state backend. The provider
  marks tokens `Sensitive: true`; protecting your state file is up to you.
- File contents leaking into state or plan output. By design the provider stores
  file content in state and prints it in `terraform plan` / `apply` diffs (and
  thus CI logs); see the Threat model section above. Do not deliver secrets
  through it.
