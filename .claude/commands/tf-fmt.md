---
description: Format Terraform examples (`just tf-fmt`) and/or verify (`just check-tf-fmt`).
allowed-tools: Bash
argument-hint: [check]
---

If `$ARGUMENTS` is `check`, run the verification only:

```bash
just check-tf-fmt
```

Otherwise apply formatting:

```bash
just tf-fmt
```

`examples/**` HCL must be `terraform fmt`-clean or CI fails. After formatting, run `just docs` if examples feed into generated docs (they do).
