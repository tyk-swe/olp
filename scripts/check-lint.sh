#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
legacy=.golangci-legacy.json
tools=$(mktemp -d)
trap 'rm -rf "$tools"' EXIT
report=$tools/report.json
GOBIN=$tools go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
status=0
"$tools/golangci-lint" run --show-stats=false --output.json.path "$report" \
  --output.text.path /dev/null ./... 2>"$tools/log" || status=$?
if ((status > 1)) || ! jq -e '.Issues | type == "array" or . == null' "$report" >/dev/null; then
  cat "$tools/log" >&2
  echo "check-lint: golangci-lint failed to run" >&2
  exit 1
fi
problems=$(jq -r --slurpfile legacy "$legacy" '
  def entry:
    if .FromLinter == "funlen" then
      {list: "functionOverMaxLines", name: "\(.Pos.Filename) \(.SourceLines[0] | capture("^func (\\((\\w+ )?\\*?(?<receiver>\\w+)[^)]*\\) )?(?<name>\\w+)") | [.receiver // empty, .name] | join("."))"}
    elif .FromLinter == "revive" and (.Text | startswith("file-length-limit")) then
      {list: "fileOverMaxLines", name: .Pos.Filename}
    else null end;
  [.Issues[] | {issue: ., entry: entry}] as $found
  | ($found | map(select(.entry as $entry
        | $entry == null or (($legacy[0][$entry.list] // []) | index($entry.name) | not))
      | "\(.issue.Pos.Filename):\(.issue.Pos.Line): \(.issue.Text) (\(.issue.FromLinter))"))
  + ($legacy[0] | to_entries | map(.key as $list | .value[] as $name
      | select($found | any(.entry == {list: $list, name: $name}) | not)
      | "\($list): \($name) no longer violates, remove it from '"$legacy"'"))
  | unique | .[]' "$report")
if [[ -n $problems ]]; then
  echo "$problems" >&2
  echo "check-lint: fix the code, never add an entry to $legacy" >&2
  exit 1
fi
./scripts/check-exemptions.sh

gateway=github.com/tyk-swe/olp/internal/gateway
declare -A gateway_imports=(
  [fault]="" [events]="" [traceingest]="" [guardstream]="" [generationrefs]=""
  [realtimeresponses]="ingress" [publishedprompt]="fault ingress"
  [health]="events" [ingress]="fault" [quota]="events fault" [models]="fault ingress"
  [codeforward]="events fault ingress quota codeforward/coderesearch codeforward/codexwire" [delegation]="fault ingress"
  [codeforward/coderesearch]="" [codeforward/codexwire]=""
  [healthprobes]="health kernel quota"
  [surface/anthropicstate]="fault kernel"
  [surface/cloudbatches]="fault ingress kernel"
  [surface/geminiinteractions]="fault kernel"
  [surface/geministate]="fault kernel"
  [surface/guardrails]="fault kernel"
  [surface/playground]="fault kernel publishedprompt"
  [surface/retrieval]="fault kernel"
  [surface/media]="fault kernel"
  [surface/openaistate]="fault generationrefs ingress kernel"
  [surface/passthrough]="fault ingress kernel"
  [surface/retained]="fault ingress kernel quota"
  [surface/bedrock]="fault kernel"
  [surface/realtime]="fault ingress kernel quota realtimeresponses"
  [surface/nativeduplex]="fault ingress kernel"
  [surface/geminilive]="fault ingress kernel quota"
  [kernel]="codeforward delegation events fault generationrefs guardstream health ingress models publishedprompt quota realtimeresponses traceingest"
)
while read -r package imports; do
  [[ $package == "$gateway" ]] && continue
  name=${package#"$gateway/"}
  if [[ ! -v gateway_imports[$name] ]]; then
    echo "check-lint: declare which gateway packages $package may import in scripts/check-lint.sh" >&2
    exit 1
  fi
  for imported in $imports; do
    [[ $imported == "$gateway/"* ]] || continue
    if [[ " ${gateway_imports[$name]} " != *" ${imported#"$gateway/"} "* ]]; then
      echo "check-lint: $package must not import $imported" >&2
      exit 1
    fi
  done
done < <(go list -f '{{.ImportPath}} {{join .Imports " "}}' ./internal/gateway/...)
