# Security policy

## Supported versions

Security fixes target the latest published 3.0.x release. The development
branch also accepts vulnerability reports, including unreleased changes.

| Version | Security support |
| --- | --- |
| 3.0.x | Latest published patch |
| Development branch | Reports accepted; fixes land before release |
| 2.x and earlier | Unsupported; 3.0 requires a fresh installation |

Report suspected vulnerabilities in any version; upgrading is not a
prerequisite for reporting. A package version in a source checkout does not
establish that the checkout is a published release.

## Reporting a vulnerability

Report suspected vulnerabilities privately to `support@mail.tyk.sh`; do not
open a public issue or pull request. Include the affected version, impact,
reproduction steps, and any mitigation. Never send production credentials,
API keys, prompts, model outputs, session cookies, or customer data.

Maintainers acknowledge reports through the same private channel within
three business days. Within fourteen days of acknowledgement you receive a
validation status and either a remediation plan or a rejection rationale;
release timing and disclosure are then coordinated according to severity and
exploitability.
