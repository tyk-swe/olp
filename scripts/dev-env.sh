# shellcheck shell=bash
if [[ -f .git ]]; then
  worktree_id=$(printf '%s' "$PWD" | cksum | cut -d ' ' -f 1)
  port_base=$((20000 + worktree_id % 3000 * 3))
  export COMPOSE_PROJECT_NAME="olp-dev-$worktree_id"
  export OLP_POSTGRES_PORT=0 OLP_VALKEY_PORT=0 OLP_CACHE_VALKEY_PORT=0
  console_port=$port_base api_port=$((port_base + 1)) observability_port=$((port_base + 2))
else
  console_port=5173 api_port=8082 observability_port=9092
fi
