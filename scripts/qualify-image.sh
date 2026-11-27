#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
candidate_image=${OLP_CANDIDATE_IMAGE:-${OLP_CONSOLE_E2E_IMAGE:-}}
: "${candidate_image:?set OLP_CANDIDATE_IMAGE to the immutable candidate image}"
[[ $candidate_image == *@sha256:* ]] || { echo 'Candidate must be selected by digest.' >&2; exit 2; }
docker pull --platform "${OLP_IMAGE_PLATFORM:?set the native image platform}" "$candidate_image"
./scripts/smoke-image-modes.sh "$candidate_image"
./scripts/scan-image.sh "$candidate_image" "${RUNNER_TEMP:-/tmp}/olp-candidate-scan"
./scripts/smoke-image-services.sh "$candidate_image" "${OLP_IMAGE_PLATFORM#linux/}"
./scripts/smoke-image-compose.sh "$candidate_image"
