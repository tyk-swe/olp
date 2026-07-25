#!/usr/bin/env bash

# Shared fail-closed primitives for repository invariant scripts.
#
# checked_rg_capture writes ripgrep's standard output to the named output
# variable and writes 1 or 0 to the named match variable. Ripgrep status 1 is a
# successful scan with no matches. Statuses greater than 1 are scan failures,
# leave both caller variables unchanged, and are returned to the caller.

validation_script_name() {
  printf '%s' "${VALIDATION_SCRIPT_NAME:-${0##*/}}"
}

validation_require_executable() {
  local executable=$1

  if ! command -v "$executable" >/dev/null 2>&1; then
    printf '%s: preflight failed: required executable %q was not found in PATH\n' \
      "$(validation_script_name)" "$executable" >&2
    return 127
  fi
}

validation_require_file() {
  local path=$1

  if [[ ! -e $path ]]; then
    printf '%s: preflight failed: required file is missing: %s\n' \
      "$(validation_script_name)" "$path" >&2
    return 2
  fi
  if [[ ! -f $path ]]; then
    printf '%s: preflight failed: required file path is invalid: %s\n' \
      "$(validation_script_name)" "$path" >&2
    return 2
  fi
  if [[ ! -r $path ]]; then
    printf '%s: preflight failed: required file is unreadable: %s\n' \
      "$(validation_script_name)" "$path" >&2
    return 2
  fi
}

validation_require_directory() {
  local path=$1

  if [[ ! -e $path ]]; then
    printf '%s: preflight failed: required directory is missing: %s\n' \
      "$(validation_script_name)" "$path" >&2
    return 2
  fi
  if [[ ! -d $path ]]; then
    printf '%s: preflight failed: required directory path is invalid: %s\n' \
      "$(validation_script_name)" "$path" >&2
    return 2
  fi
  if [[ ! -r $path || ! -x $path ]]; then
    printf '%s: preflight failed: required directory is unreadable: %s\n' \
      "$(validation_script_name)" "$path" >&2
    return 2
  fi
}

checked_rg_capture() {
  local output_variable=$1
  local match_variable=$2
  local operation=$3
  local path=$4
  shift 4

  if [[ ! $output_variable =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ||
        ! $match_variable =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ||
        $output_variable == "$match_variable" ]]; then
    printf '%s: checked ripgrep invocation has invalid output variables: operation=%s path=%s\n' \
      "$(validation_script_name)" "$operation" "$path" >&2
    return 2
  fi

  validation_require_executable rg || return $?

  local captured_output
  local rg_status
  local rg_match_result
  if captured_output=$(rg "$@"); then
    rg_status=0
    rg_match_result=1
  else
    rg_status=$?
    if (( rg_status == 1 )); then
      rg_match_result=0
    else
      printf '%s: ripgrep scan failed: operation=%s path=%s exit=%d\n' \
        "$(validation_script_name)" "$operation" "$path" "$rg_status" >&2
      return "$rg_status"
    fi
  fi

  printf -v "$output_variable" '%s' "$captured_output"
  printf -v "$match_variable" '%s' "$rg_match_result"
}

checked_rg_match() {
  local result_variable=$1
  local operation=$2
  local path=$3
  shift 3

  local ignored_output=
  local internal_match_result=
  checked_rg_capture ignored_output internal_match_result \
    "$operation" "$path" "$@" || return $?
  : "$ignored_output"
  printf -v "$result_variable" '%s' "$internal_match_result"
}
