#!/usr/bin/env bash
set -euo pipefail

# The Makefile is the task index: every command CI runs must exist there (or
# under scripts/ci/) so a developer can run the same step locally and the two
# cannot drift. A single-line `run:` therefore has to start with `make ` or
# `./scripts/ci/`. Multi-line `run: |` blocks are runner-only setup with no
# local equivalent and are allowed only by step name, listed below.

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/lib/repository-validation.sh
source "$script_dir/lib/repository-validation.sh"
VALIDATION_SCRIPT_NAME=check-ci-make-lockstep.sh

workflow=${CI_WORKFLOW:-.github/workflows/ci.yml}
validation_require_file "$workflow"

allowed_multiline_steps=(
  'Install lint dependencies'
  'Lint GitHub Actions workflows'
  'Install boundary-check dependencies'
  'Derive the pinned fuzz toolchain'
  'Require every required-tier job to succeed'
  'Require every required and full-tier job to succeed'
  'Run two-gateway limits, LKG, missed-hint, and revocation proof'
)

step_allowed_multiline() {
  local name=$1 allowed
  for allowed in "${allowed_multiline_steps[@]}"; do
    [[ $name == "$allowed" ]] && return 0
  done
  return 1
}

violations=()
step_name=""
line_number=0
while IFS= read -r line || [[ -n $line ]]; do
  line_number=$((line_number + 1))
  if [[ $line =~ ^[[:space:]]*-[[:space:]]name:[[:space:]]*(.*)$ ]]; then
    step_name=${BASH_REMATCH[1]}
    continue
  fi
  [[ $line =~ ^[[:space:]]*run:[[:space:]]*(.*)$ ]] || continue
  command=${BASH_REMATCH[1]}
  # A bare `run:` is the `defaults.run` mapping, not a step.
  [[ -z $command ]] && continue
  case $command in
    '|' | '|-' | '>' | '>-')
      step_allowed_multiline "$step_name" \
        || violations+=("$line_number: multi-line step '$step_name' is not in the allow-list")
      ;;
    'make '* | './scripts/ci/'*) ;;
    *)
      violations+=("$line_number: step '$step_name' runs '$command' instead of a make target")
      ;;
  esac
done < "$workflow"

if ((${#violations[@]} > 0)); then
  echo "$workflow steps must run through make (see scripts/check-ci-make-lockstep.sh):" >&2
  printf '  %s\n' "${violations[@]}" >&2
  exit 1
fi
echo "ci.yml command steps are in lockstep with the Makefile"
