# shellcheck shell=bash
# Disposable PostgreSQL databases for integration runs. Requires
# OLP_TEST_DATABASE_ADMIN_URL, OLP_TEST_DATABASE_URL_PREFIX and a short
# lower-case alphanumeric OLP_TEST_RUN_TOKEN.
: "${OLP_TEST_DATABASE_ADMIN_URL:?set the disposable database admin URL}"
: "${OLP_TEST_DATABASE_URL_PREFIX:?set the disposable database URL prefix}"
: "${OLP_TEST_RUN_TOKEN:?set the integration run token}"
[[ $OLP_TEST_RUN_TOKEN =~ ^[a-z0-9]{1,10}$ ]] || exit 2

create_disposable_database() {
  psql "$OLP_TEST_DATABASE_ADMIN_URL" -Xq -v ON_ERROR_STOP=1 -c "CREATE DATABASE $1"
}

drop_disposable_database() {
  psql "$OLP_TEST_DATABASE_ADMIN_URL" -Xq -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS $1 WITH (FORCE)" >/dev/null
}
