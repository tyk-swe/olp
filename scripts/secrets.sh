# shellcheck shell=bash
python3 - "${1:?secret directory required}" <<'PY' || return
import json, os, secrets, stat, sys
from pathlib import Path

directory = Path(sys.argv[1])
directory.mkdir(parents=True, exist_ok=True, mode=0o700)
os.chmod(directory, 0o700)
for name, value in {
    'auth.key': secrets.token_hex(32),
    'bootstrap.token': secrets.token_urlsafe(32),
    'master.json': json.dumps({'active_version': 1, 'keys': [{'version': 1, 'key': secrets.token_hex(32)}]})
}.items():
    path = directory / name
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError:
        if not stat.S_ISREG(path.lstat().st_mode):
            sys.exit(f'cannot keep {path}: expected a regular file')
        continue
    with os.fdopen(fd, 'w') as stream:
        stream.write(value + '\n')
PY
export OLP_AUTH_HMAC_KEY_FILE=${OLP_AUTH_HMAC_KEY_FILE:-$1/auth.key}
export OLP_BOOTSTRAP_TOKEN_FILE=${OLP_BOOTSTRAP_TOKEN_FILE:-$1/bootstrap.token}
export OLP_MASTER_KEY_FILE=${OLP_MASTER_KEY_FILE:-$1/master.json}
