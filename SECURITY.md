# Security policy

## Report a vulnerability

Do not open a public GitHub issue for a security problem.
Use one of these private channels:

- Best channel: open a GitHub Security Advisory at
  <https://github.com/greeddj/terraform-provider-gitlabcommits/security/advisories/new>.
- Other channel: send an email to the maintainer at <greeddj@gmail.com>.

Give this information in the report:

- A description of the problem and of its effect.
- The smallest HCL configuration that shows the problem, and the steps that cause it.
- The output of `terraform version` in the initialized working directory, and the GitLab version.

The maintainer sends a first reply in 7 days or less.
If the maintainer finds that the problem is real, you and the maintainer agree on a disclosure date before the fix is public.

## Scope

In scope: the provider in this repository, its release files and the GitLab API requests that it sends.

Out of scope:

- Vulnerabilities in GitLab. Report them through the GitLab HackerOne program at <https://hackerone.com/gitlab>.
- Vulnerabilities in Terraform Core or in the Terraform Plugin Framework.

## Threat model

The provider manages configuration files that are not secret, for example Argo CD manifests, Helm values files and GitLab CI files.

### File content

The provider does not mark file content as sensitive, because people must examine the content in the plan.

- Plan output, apply output and CI logs show `content` and `content_base64` of the `gitlabcommits_files` resource. Terraform hides only sensitive values, for example a value from a sensitive variable or from the `sensitive()` function.
- Terraform state holds the content of each managed file in plain text, also sensitive content.
- The `gitlabcommits_file` data source also stores the file content in state: `content_base64`, and `content` for UTF-8 text.

Do not put secrets in managed files.
Keep each secret in a secret store, for example Sealed Secrets, External Secrets Operator or Vault.
Refer to the secret by its name in the managed file.
Use a state backend that controls access and encrypts the data.
For more information, see [File content in state](docs/resources/files.md#file-content-in-state).

### Token

The provider marks `token` as sensitive and writes no token into state or into its log entries.
It does not follow a redirect to another host or port, or from `https` to `http`, because each request contains the token.
For token types, permissions and `base_url`, see [Authentication](docs/index.md#authentication).

## Problems that we do not fix

- A token that your HCL configuration or your state backend exposes. Protect the configuration and the state backend.
- File content in plan output, apply output, CI logs or state. This is by design (see [File content](#file-content)).
