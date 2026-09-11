use crate::{database, database::error::Error, test_support::TestDb};

#[tokio::test]
#[ignore = "requires the integration PostgreSQL service"]
async fn legacy_storage_is_refused_before_any_v3_objects_are_created() {
    for schema in ["public", "previous_installation"] {
        let db = TestDb::create_empty("v2refuse").await;
        let pool = db.pool(1).await;
        if schema != "public" {
            sqlx::query("CREATE SCHEMA previous_installation")
                .execute(&pool)
                .await
                .unwrap();
        }
        let sql = if schema == "public" {
            "CREATE TABLE public.installation_identity (sentinel text); INSERT INTO public.installation_identity VALUES ('preserve-me')"
        } else {
            "CREATE TABLE previous_installation.installation_identity (sentinel text); INSERT INTO previous_installation.installation_identity VALUES ('preserve-me')"
        };
        sqlx::raw_sql(sql).execute(&pool).await.unwrap();
        assert!(matches!(
            database::migrate(&pool).await,
            Err(Error::LegacyInstallation)
        ));
        assert!(matches!(
            database::connect(db.url(), 1).await,
            Err(Error::LegacyInstallation)
        ));
        let created = sqlx::query_scalar::<_, bool>(
            "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'olp_v3')",
        )
        .fetch_one(&pool)
        .await
        .unwrap();
        assert!(!created);
        let sentinel_sql = if schema == "public" {
            "SELECT sentinel FROM public.installation_identity"
        } else {
            "SELECT sentinel FROM previous_installation.installation_identity"
        };
        let sentinel = sqlx::query_scalar::<_, String>(sentinel_sql)
            .fetch_one(&pool)
            .await
            .unwrap();
        assert_eq!(sentinel, "preserve-me");
        pool.close().await;
    }
}

#[tokio::test]
#[ignore = "requires the integration PostgreSQL service"]
async fn fresh_installations_migrate_idempotently_and_have_distinct_namespaces() {
    let first = TestDb::create_migrated("v3first").await;
    let second = TestDb::create_migrated("v3second").await;
    let first_pool = first.pool(1).await;
    let second_pool = second.pool(1).await;
    database::migrate(&first_pool).await.unwrap();
    let first_keys = crate::limits::valkey::valkey_keyspace(&first_pool)
        .await
        .unwrap();
    let second_keys = crate::limits::valkey::valkey_keyspace(&second_pool)
        .await
        .unwrap();
    assert!(first_keys.prefix().starts_with("olp:3:"));
    assert_ne!(first_keys.prefix(), second_keys.prefix());
    let applied =
        sqlx::query_scalar::<_, i64>("SELECT count(*) FROM olp_v3._sqlx_migrations WHERE success")
            .fetch_one(&first_pool)
            .await
            .unwrap();
    assert_eq!(applied, 5);
    first_pool.close().await;
    second_pool.close().await;
}

#[tokio::test]
#[ignore = "requires the integration PostgreSQL service"]
async fn runtime_deadlines_bound_queries_and_lock_waits_without_poisoning_connections() {
    let db = TestDb::create_migrated("deadlines").await;
    let pool = db.pool(2).await;
    let settings = sqlx::query_as::<_, (String, String, String)>(
        "SELECT current_setting('statement_timeout'), current_setting('lock_timeout'), current_setting('idle_in_transaction_session_timeout')")
        .fetch_one(&pool).await.unwrap();
    assert_eq!(settings, ("30s".into(), "5s".into(), "1min".into()));
    let mut connection = pool.acquire().await.unwrap();
    sqlx::raw_sql("SET statement_timeout = '50ms'; SET lock_timeout = '25ms'")
        .execute(&mut *connection)
        .await
        .unwrap();
    let error = sqlx::query("SELECT pg_sleep(1)")
        .execute(&mut *connection)
        .await
        .unwrap_err();
    assert_eq!(
        error.as_database_error().unwrap().code().as_deref(),
        Some("57014")
    );
    let mut held = pool.begin().await.unwrap();
    sqlx::query("SELECT id FROM installation_identity FOR UPDATE")
        .execute(&mut *held)
        .await
        .unwrap();
    let error = sqlx::query("SELECT id FROM installation_identity FOR UPDATE")
        .execute(&mut *connection)
        .await
        .unwrap_err();
    assert_eq!(
        error.as_database_error().unwrap().code().as_deref(),
        Some("55P03")
    );
    held.rollback().await.unwrap();
    sqlx::query("SELECT 1")
        .execute(&mut *connection)
        .await
        .unwrap();
}

#[tokio::test]
#[ignore = "requires the integration PostgreSQL service"]
async fn runtime_role_can_use_application_tables_but_cannot_change_schema_or_identity() {
    let db = TestDb::create_migrated("roles").await;
    let pool = db.pool(1).await;
    let mut transaction = pool.begin().await.unwrap();
    let role = format!("olp_runtime_{}", uuid::Uuid::now_v7().simple());
    sqlx::raw_sql(sqlx::AssertSqlSafe(format!(
        "CREATE ROLE {role} NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT"
    )))
    .execute(&mut *transaction)
    .await
    .unwrap();
    let grants = include_str!("../../scripts/grant-runtime-database-role.sql")
        .replace("\\set ON_ERROR_STOP on\n", "")
        .replace("BEGIN;", "")
        .replace("COMMIT;", "")
        .replace(":\"runtime_role\"", &role);
    sqlx::raw_sql(sqlx::AssertSqlSafe(grants))
        .execute(&mut *transaction)
        .await
        .unwrap();
    sqlx::raw_sql(sqlx::AssertSqlSafe(format!("SET LOCAL ROLE {role}")))
        .execute(&mut *transaction)
        .await
        .unwrap();
    sqlx::query("SELECT id FROM installation_identity")
        .fetch_one(&mut *transaction)
        .await
        .unwrap();
    sqlx::query("UPDATE settings SET value = value WHERE false")
        .execute(&mut *transaction)
        .await
        .unwrap();
    for statement in [
        "CREATE TABLE public.forbidden (id int)",
        "CREATE TABLE olp_v3.forbidden (id int)",
        "DELETE FROM olp_v3._sqlx_migrations",
        "UPDATE olp_v3.installation_identity SET id = gen_random_uuid()",
    ] {
        sqlx::query("SAVEPOINT permission_check")
            .execute(&mut *transaction)
            .await
            .unwrap();
        let error = sqlx::raw_sql(sqlx::AssertSqlSafe(statement))
            .execute(&mut *transaction)
            .await
            .unwrap_err();
        assert_eq!(
            error.as_database_error().unwrap().code().as_deref(),
            Some("42501")
        );
        sqlx::query("ROLLBACK TO SAVEPOINT permission_check")
            .execute(&mut *transaction)
            .await
            .unwrap();
    }
    transaction.rollback().await.unwrap();
}
