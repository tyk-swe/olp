#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

base=$(git merge-base HEAD "${EXEMPTIONS_BASE:-origin/main}")
renames=$(git diff -M --diff-filter=R --name-status "$base" |
  jq -Rn '[inputs | split("\t") | {key: .[2], value: .[1]}] | from_entries')

check() {
  local list=$1 root=$2 before added
  before=$(git show "$base:$list" 2>/dev/null) || return 0
  added=$(jq -r --argjson before "$before" --argjson renames "$renames" --arg root "$root" '
    to_entries[] | .key as $list | select($before[$list] != null) | .value[] as $entry
    | ($entry | split(" ")) as $words
    | ([($renames[$root + $words[0]] // "" | ltrimstr($root))] + $words[1:] | join(" ")) as $renamed
    | select($before[$list] | (index($entry) or index($renamed)) | not)
    | "\($list): \($entry)"' "$list")
  [[ -z $added ]] && return 0
  printf 'check-exemptions: %s gained an entry\n%s\n' "$list" "$added" >&2
  echo "check-exemptions: legacy lists only shrink. Fix the new violation instead of listing it." >&2
  return 1
}

check console/eslint-legacy.json console/
