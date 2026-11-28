#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C
shopt -s nullglob
cd "$(dirname "$0")/.."

baseline=scripts/check-tables.baseline
table='"?olp"?\."?[a-z0-9_]+"?'
list="$table([[:space:]]*,[[:space:]]*(only[[:space:]]+)?$table)*"
definition="\b(create([[:space:]]+unlogged)?[[:space:]]+table([[:space:]]+if[[:space:]]+not[[:space:]]+exists)?"
definition+="|drop[[:space:]]+table([[:space:]]+if[[:space:]]+exists)?"
definition+="|alter[[:space:]]+table([[:space:]]+if[[:space:]]+exists)?([[:space:]]+only)?)"
definition+="[[:space:]]+$list([[:space:]]+rename[[:space:]]+to[[:space:]]+\"?[a-z0-9_]+)?"
write="\b(insert[[:space:]]+into|update|delete[[:space:]]+from|merge[[:space:]]+into"
write+="|truncate([[:space:]]+table)?|copy)[[:space:]]+(only[[:space:]]+)?$list"
write+="|identifier\{[[:space:]]*\"olp\",[[:space:]]*\"[a-z0-9_]+\""

tables=$(
  cat /dev/null internal/platform/database/migrations/*.sql |
    grep -zoiE "$definition" |
    tr '\t\r\n\0' '   \n' |
    tr -d '"' |
    tr '[:upper:]' '[:lower:]' |
    awk '
      {
        renamed = match($0, / rename +to +/) ? substr($0, RSTART + RLENGTH) : ""
        if ($1 == "alter" && renamed == "") next
        rest = $0
        while (match(rest, /olp\.[a-z0-9_]+/)) {
          name = substr(rest, RSTART + 4, RLENGTH - 4)
          rest = substr(rest, RSTART + RLENGTH)
          if ($1 == "create") live[name] = 1
          else delete live[name]
          if ($1 == "alter") live[renamed] = 1
        }
      }
      END { for (name in live) print name }
    '
)

owners=$(
  awk 'NF { module = FILENAME; sub(/\/tables\.txt$/, "", module); print $0 "\t" module }' \
    /dev/null internal/*/tables.txt
)

problems=$(
  awk -F'\t' '
    $0 == "" { next }
    FILENAME == ARGV[1] { count[$1] = 0; next }
    !($1 in count) { print $2 "/tables.txt names " $1 ", which the migrations do not create"; next }
    { count[$1]++; modules[$1] = modules[$1] " " $2 }
    END {
      for (name in count) {
        if (count[name] == 0) print "olp." name " has no owner, add it to the tables.txt of one module"
        if (count[name] > 1) print "olp." name " has more than one owner:" modules[name]
      }
    }
  ' <(echo "$tables") <(echo "$owners") | sort
)
if [[ -n $problems ]]; then
  printf '%s\n' "$problems" >&2
  exit 1
fi

writes=$(
  { grep -rzoiHE --include='*.go' --exclude='*_test.go' --exclude-dir='.?*' --exclude-dir=node_modules \
    "$write" . || (($? == 1)); } |
    tr '\t\r\n\0' '   \n' |
    awk -v OFS='\t' '
      {
        colon = index($0, ":")
        file = substr($0, 3, colon - 3)
        if (file ~ /^tests\//) next
        sql = tolower(substr($0, colon + 1))
        sub(/^identifier\{ *"olp", *"/, "olp.", sql)
        gsub(/"/, "", sql)
        module = split(file, part, "/") > 2 ? part[1] "/" part[2] : part[1]
        while (match(sql, /olp\.[a-z0-9_]+/)) {
          print module, substr(sql, RSTART + 4, RLENGTH - 4), file
          sql = substr(sql, RSTART + RLENGTH)
        }
      }
    '
)

foreign=$(
  awk -F'\t' '
    FILENAME == ARGV[1] { owner[$1] = $2; next }
    $2 in owner && owner[$2] != $1 { print $1 " " $2 "\t" $3 "\t" owner[$2] }
  ' <(echo "$owners") <(echo "$writes") | sort
)

problems=$(
  awk -F'\t' -v baseline="$baseline" '
    $0 == "" { next }
    FILENAME == ARGV[1] {
      fields = split($0, field, " ")
      pair = field[1] " " field[2]
      if (fields != 3 || field[3] !~ /^[1-9][0-9]*$/ || pair in allowed) {
        print baseline " has a malformed or repeated line \"" $0 "\""
      } else {
        allowed[pair] = field[3] + 0
        listed[++lines] = pair
      }
      next
    }
    { count[$1]++ }
    !seen[$0]++ { rows[++total] = $0 }
    END {
      for (i = 1; i <= total; i++) {
        split(rows[i], column, "\t")
        pair = column[1]
        limit = (pair in allowed) ? allowed[pair] : 0
        if (count[pair] <= limit) continue
        split(pair, part, " ")
        more = limit ? " (" part[1] " has " count[pair] " such writes, " limit " allowed)" : ""
        print column[2] " writes olp." part[2] ", which " column[3] " owns" more
        grown = 1
      }
      if (grown) print "move the SQL into the owning module and pass it the pgx.Tx of the caller, do not add to " baseline
      for (i = 1; i <= lines; i++) {
        pair = listed[i]
        if (count[pair] + 0 >= allowed[pair]) continue
        fix = count[pair] ? "change it to \"" pair " " count[pair] "\"" : "delete it"
        print baseline " has the stale line \"" pair " " allowed[pair] "\", " fix
      }
    }
  ' "$baseline" <(echo "$foreign")
)
if [[ -n $problems ]]; then
  printf '%s\n' "$problems" >&2
  exit 1
fi
