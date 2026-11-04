#!/usr/bin/env bash
set -euo pipefail
.local/bin/olp migrate
exec .local/bin/olp all
