#!/usr/bin/env bash
# A tripwire for accidental Rust use by ordinary Go workflows. Images provide
# the stronger qualification: their build stages contain no Rust toolchain.
set -euo pipefail
guard=$(mktemp -d)
trap 'rm -rf -- "$guard"' EXIT
for tool in cargo rustc rustup; do
  cat > "$guard/$tool" <<'SH'
#!/usr/bin/env bash
echo 'Rust invocation is forbidden in the Go workflow' >&2
exit 97
SH
  chmod +x "$guard/$tool"
done
PATH="$guard:$PATH" "$@"
