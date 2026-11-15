# Pinned signing keys

`scripts/lib/verified_install.sh` checks the downloads it installs against
these keys. It also checks each key file's fingerprint before using it (#46).

| File | Signs | Fingerprint | Expires | Source |
|---|---|---|---|---|
| `amazon-ssm-agent.asc` | SSM Agent RPMs (3.3.4851.0+) | `1C5B 395C BE5E 74E2 F1CA 2159 2784 DBF3 88D1 9D46` | 2027-11-25 | [Verifying the signature of SSM Agent](https://docs.aws.amazon.com/systems-manager/latest/userguide/verify-agent-signature.html) |
| `aws-cli.asc` | AWS CLI v2 installers | `FB5D B77F D5C1 18B8 0511 ADA8 A631 0ACC 4672 475C` | 2027-07-01 | [Installing the AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html) |
| `globus-2024.asc` | Globus Connect Server RPMs and repo metadata | `FD95 28E6 EE73 1D82 64C2 95D7 C934 C57C 39AF 3E61` | none | `RPM-GPG-KEY-Globus-2024` in Globus's `globus-repo` RPM |

**Rotating a key:**
1. Copy the new public key from its source page.
2. Check that `gpg --show-keys --with-fingerprint <file>` matches the fingerprint the page publishes.
3. Verify a current download's detached signature with it.
4. Update the fingerprint constant in `scripts/lib/verified_install.sh`.

Builds fail closed once a key expires or is rotated, so check the expiry dates
above before then.
