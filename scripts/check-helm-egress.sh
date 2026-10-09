#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
args=(--set networkPolicy.enabled=true --set networkPolicy.edge.cidrs[0]=192.0.2.0/24
  --set networkPolicy.egress.restricted=true
  --set networkPolicy.egress.postgresql.cidrs[0]=10.10.0.0/16
  --set networkPolicy.egress.valkey.cidrs[0]=10.11.0.0/16)
if helm template olp deploy/helm "${args[@]}" >/dev/null 2>&1; then
  echo 'Restricted DNS must require explicit resolver CIDRs' >&2
  exit 1
fi
rendered=$(helm template olp deploy/helm "${args[@]}" --set networkPolicy.egress.dns.cidrs[0]=10.96.0.10/32)
# Every enabled component, including migration, must bind its TCP/UDP DNS
# ports to the configured resolver. Adjacent block matching catches a ports-only
# rule even if a resolver CIDR happens to appear elsewhere in the document.
expected='    - ports:
      - port: 53
        protocol: UDP
      - port: 53
        protocol: TCP
      to:
      - ipBlock:
          cidr: 10.96.0.10/32'
count=$(awk -v expected="$expected" 'BEGIN { RS="---" } index($0, expected) { n++ } END { print n+0 }' <<<"$rendered")
if [[ "$count" != 4 ]]; then
  echo "Expected four DNS rules bound to the resolver, got $count" >&2
  exit 1
fi
if helm template olp deploy/helm "${args[@]}" --set networkPolicy.egress.dns.enabled=false | grep -q 'port: 53'; then
  echo 'Disabled DNS must not emit a DNS port rule' >&2
  exit 1
fi
helm template olp deploy/helm >/dev/null
helm template olp deploy/helm --set networkPolicy.egress.dns.cidrs[0]=invalid >/dev/null 2>&1 && {
  echo 'Invalid resolver CIDRs must fail schema validation' >&2
  exit 1
}
echo 'Restricted DNS egress checks passed.'
