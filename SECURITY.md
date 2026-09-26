# Security policy

## Supported versions

OpenLLMProxy is a 0.x work in progress. Security fixes target the latest
published 0.x release only; earlier releases receive no fixes. The development
branch also accepts vulnerability reports, including unreleased changes.

| Version | Security support |
| --- | --- |
| Latest 0.x release | Supported |
| Earlier 0.x releases | Unsupported |
| Development branch | Reports accepted; fixes land before release |

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
