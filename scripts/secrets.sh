#!/usr/bin/env bash
# Source this helper to provision private local/test secrets without printing them.
go_secrets_dir=${1:?secret directory required}
python3 - "$go_secrets_dir" <<'PY'
import json, os, secrets, sys
from pathlib import Path
directory = Path(sys.argv[1])
directory.mkdir(parents=True, exist_ok=True, mode=0o700)
for name, value in {
    'auth.key': secrets.token_hex(32),
    'bootstrap.token': secrets.token_urlsafe(32),
    'master.json': json.dumps({'active_version': 1, 'keys': [{'version': 1, 'key': secrets.token_hex(32)}]})
}.items():
    path = directory / name
    if not path.exists():
        with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as stream:
            stream.write(value + '\n')
PY
export OLP_AUTH_HMAC_KEY_FILE=${OLP_AUTH_HMAC_KEY_FILE:-$go_secrets_dir/auth.key}
export OLP_BOOTSTRAP_TOKEN_FILE=${OLP_BOOTSTRAP_TOKEN_FILE:-$go_secrets_dir/bootstrap.token}
export OLP_MASTER_KEY_FILE=${OLP_MASTER_KEY_FILE:-$go_secrets_dir/master.json}
