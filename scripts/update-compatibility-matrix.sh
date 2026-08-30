#!/usr/bin/env bash
# Regenerates the surface x operation x provider table in
# docs/compatibility.md from the endpoint registry and the certification
# policy, by splicing the export example's stdout between the generated
# markers. Everything outside the markers is handwritten and preserved.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$script_dir/.." && pwd)
# shellcheck source=scripts/lib/repository-validation.sh
source "$script_dir/lib/repository-validation.sh"
VALIDATION_SCRIPT_NAME=update-compatibility-matrix.sh
cd "$root"

for required_executable in cargo awk grep mktemp; do
  validation_require_executable "$required_executable"
done

document=docs/compatibility.md
start_marker='<!-- generated:compatibility:start -->'
end_marker='<!-- generated:compatibility:end -->'
validation_require_file "$document"

# grep -c exits 1 on a clean no-match, which is a legitimate count of zero
# here rather than a scan failure; anything above 1 is a real failure.
count_marker() {
  local marker=$1 count status
  count=$(grep -c -F -x -- "$marker" "$document") && status=0 || status=$?
  case $status in
    0 | 1) printf '%s' "$count" ;;
    *)
      printf '%s: marker scan failed: exit=%d marker=%s\n' \
        "$(validation_script_name)" "$status" "$marker" >&2
      exit "$status"
      ;;
  esac
}

start_count=$(count_marker "$start_marker")
end_count=$(count_marker "$end_marker")
if ((start_count != 1 || end_count != 1)); then
  printf '%s: %s must contain each generated marker exactly once: start=%s end=%s\n' \
    "$(validation_script_name)" "$document" "$start_count" "$end_count" >&2
  exit 1
fi

start_line=$(grep -n -F -x -- "$start_marker" "$document" | cut -d: -f1)
end_line=$(grep -n -F -x -- "$end_marker" "$document" | cut -d: -f1)
if ((start_line >= end_line)); then
  printf '%s: the generated markers in %s are inverted: start=%s end=%s\n' \
    "$(validation_script_name)" "$document" "$start_line" "$end_line" >&2
  exit 1
fi

generated=
merged=
cleanup() { rm -f "$generated" "$merged"; }
trap cleanup EXIT
generated=$(mktemp)
merged=$(mktemp)

cargo run --locked --quiet -p olp --example export_compatibility > "$generated"

if [[ $(head -n 1 "$generated") != "$start_marker" ]] \
  || [[ $(tail -n 1 "$generated") != "$end_marker" ]]; then
  printf '%s: the export example did not emit a marked section\n' \
    "$(validation_script_name)" >&2
  exit 1
fi

# The generated output carries both markers, so the splice replaces the old
# block inclusively and copies every handwritten line untouched.
awk -v start="$start_marker" -v end="$end_marker" -v generated="$generated" '
  skipping {
    if ($0 == end) { skipping = 0 }
    next
  }
  $0 == start {
    skipping = 1
    while ((getline line < generated) > 0) { print line }
    close(generated)
    next
  }
  { print }
' "$document" > "$merged"

cat "$merged" > "$document"
echo "regenerated the compatibility matrix in $document"
