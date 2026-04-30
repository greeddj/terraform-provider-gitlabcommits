# Security policy

## Reporting a vulnerability

If you believe you've found a security issue in this provider, **do not open
a public GitHub issue**. Instead, email the maintainer directly:

- greeddj@gmail.com

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

## What we won't fix

- Token exposure caused by the user's HCL or state backend. The provider
  marks tokens `Sensitive: true`; protecting your state file is up to you.
- File contents leaking into state. By design the provider stores file
  content as part of resource state — do not commit secrets through it.
