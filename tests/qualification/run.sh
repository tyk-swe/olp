#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: tests/qualification/run.sh TARGET

Blocking targets:
  clean-install   Empty-volume Compose and Kind/Helm installation proof
  backup-restore  Seeded, quiesced PostgreSQL backup and functional restore
  n-minus-one     Strict previous-release migration and startup rehearsal
  load            100 requests/s for 2 minutes after a 30-second warm-up
  soak            50 requests/s for 30 minutes plus process resource bounds
EOF
}

if [[ $# -ne 1 || ${1:-} == --help || ${1:-} == -h ]]; then
  usage
  [[ $# -eq 1 ]] && exit 0 || exit 2
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
case "$1" in
  clean-install)
    "$root/tests/qualification/compose-clean-install.sh"
    "$root/tests/qualification/helm-clean-install.sh"
    ;;
  backup-restore) "$root/tests/qualification/backup-restore.sh" ;;
  n-minus-one) "$root/tests/qualification/n-minus-one.sh" ;;
  load | soak) "$root/tests/qualification/performance.sh" "$1" ;;
  *) usage; exit 2 ;;
esac
