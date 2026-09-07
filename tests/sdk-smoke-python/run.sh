#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
uv sync --project tests/sdk-smoke-python --frozen
exec tests/sdk-smoke/run.sh uv run --project tests/sdk-smoke-python --frozen python tests/sdk-smoke-python/smoke.py
