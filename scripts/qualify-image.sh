#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${OLP_CONSOLE_E2E_IMAGE:?set the immutable candidate image}"
[[ $OLP_CONSOLE_E2E_IMAGE == *@sha256:* ]] || { echo 'Candidate must be selected by digest.' >&2; exit 2; }
docker pull --platform "${OLP_IMAGE_PLATFORM:?set the native image platform}" "$OLP_CONSOLE_E2E_IMAGE"
./scripts/smoke-image-modes.sh "$OLP_CONSOLE_E2E_IMAGE"
./scripts/scan-image.sh "$OLP_CONSOLE_E2E_IMAGE" "${RUNNER_TEMP:-/tmp}/olp-candidate-scan"
./scripts/smoke-image-services.sh "$OLP_CONSOLE_E2E_IMAGE" "${OLP_IMAGE_PLATFORM#linux/}"
