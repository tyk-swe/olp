#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
"$script_dir/install-postgres-client.sh" 18
sudo apt-get -o Acquire::Retries=3 install --yes --no-install-recommends redis-tools
