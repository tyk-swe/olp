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
    assert_eq!(applied, 1);
    first_pool.close().await;
    second_pool.close().await;
}
