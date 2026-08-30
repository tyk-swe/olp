#!/usr/bin/env bash
set -euo pipefail

if [[ -z ${GITHUB_ENV:-} ]]; then
  echo "GITHUB_ENV is required" >&2
  exit 1
fi

credentials_path=${GOOGLE_GHA_CREDS_PATH:-}
if [[ -n $credentials_path ]]; then
  credentials_directory=$(dirname -- "$credentials_path")
  credentials_name=$(basename -- "$credentials_path")
  if [[ -z ${GITHUB_WORKSPACE:-} ]] \
    || [[ $credentials_directory != "$GITHUB_WORKSPACE" ]] \
    || [[ ! $credentials_name =~ ^gha-creds-[a-z0-9]{16}\.json$ ]]; then
    echo "refusing to remove an unexpected Google credentials path" >&2
    exit 1
  fi
  rm -f -- "$credentials_path"
fi

for variable in \
  AWS_ACCESS_KEY_ID \
  AWS_SECRET_ACCESS_KEY \
  AWS_SESSION_TOKEN \
  GOOGLE_APPLICATION_CREDENTIALS \
  CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE; do
  printf '%s=\n' "$variable" >>"$GITHUB_ENV"
done
