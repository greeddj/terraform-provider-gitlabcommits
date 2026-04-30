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

## What we won't fix

- Token exposure caused by the user's HCL or state backend. The provider
  marks tokens `Sensitive: true`; protecting your state file is up to you.
- File contents leaking into state. By design the provider stores file
  content as part of resource state — do not commit secrets through it.
