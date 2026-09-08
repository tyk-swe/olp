# shellcheck shell=bash
# Shared hardening for running a candidate OLP image on the host network:
# the current user, a read-only root, a bounded /tmp and read-only bind mounts
# of the key files named by the OLP_*_FILE variables.
olp_image_container_args() {
  local name path
  olp_image_args=(--network host --user "$(id -u):$(id -g)" --read-only
    --tmpfs /tmp:rw,nosuid,nodev,size=128m)
  for name in OLP_MASTER_KEY_FILE OLP_AUTH_HMAC_KEY_FILE OLP_BOOTSTRAP_TOKEN_FILE; do
    path=${!name}
    olp_image_args+=(--mount "type=bind,src=$path,dst=$path,readonly")
  done
}
