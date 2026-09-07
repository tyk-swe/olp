use olp::test_support::TestDb;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn issuer_filter_precedes_pagination_and_keeps_inactive_attribution() {
    let db = TestDb::create_migrated("api_key_issuers").await;
    let pool = db.pool(2).await;
    let issuers = [Uuid::now_v7(), Uuid::now_v7()];
    for (index, issuer) in issuers.iter().enumerate() {
        sqlx::query("INSERT INTO users (id, email, display_name, role, active) VALUES ($1, $2, 'Issuer', 'developer', $3)")
            .bind(issuer).bind(format!("issuer-{index}@keys.test")).bind(index == 0)
            .execute(&pool).await.unwrap();
    }
    let mut ids: Vec<Uuid> = (0..4).map(|_| Uuid::now_v7()).collect();
    ids.sort();
    for (index, id) in ids.iter().enumerate() {
        sqlx::query("INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by, daily_cost_limit, revoked_at) VALUES ($1, $2, $3, 'Attributed key', $4, 2.25, CASE WHEN $5 THEN now() ELSE NULL END)")
            .bind(id).bind(format!("issuer_key_{index}")).bind(vec![0u8; 32])
            .bind(issuers[index % 2]).bind(index == 3)
            .execute(&pool).await.unwrap();
    }
    for (index, issuer) in issuers.iter().enumerate() {
        let first = olp::access::api_keys::repository::list_api_keys(&pool, Some(*issuer), None, 1)
            .await
            .unwrap();
        assert_eq!(first.items.len(), 1);
        assert_eq!(first.items[0].id, ids[index]);
        assert_eq!(first.items[0].created_by, *issuer);
        assert_eq!(
            first.items[0].created_by_email,
            format!("issuer-{index}@keys.test")
        );
        assert_eq!(
            first.items[0]
                .daily_cost_limit
                .unwrap()
                .normalize()
                .to_string(),
            "2.25"
        );
        assert_eq!(first.next_cursor, Some(ids[index]));
        let second = olp::access::api_keys::repository::list_api_keys(
            &pool,
            Some(*issuer),
            first.next_cursor,
            1,
        )
        .await
        .unwrap();
        assert_eq!(second.items.len(), 1);
        assert_eq!(second.items[0].id, ids[index + 2]);
        assert_eq!(second.items[0].revoked_at.is_some(), index == 1);
        assert!(second.next_cursor.is_none());
        let exhausted = olp::access::api_keys::repository::list_api_keys(
            &pool,
            Some(*issuer),
            Some(ids[index + 2]),
            1,
        )
        .await
        .unwrap();
        assert!(exhausted.items.is_empty());
        assert!(exhausted.next_cursor.is_none());
    }
    let unfiltered = olp::access::api_keys::repository::list_api_keys(&pool, None, None, 2)
        .await
        .unwrap();
    assert_eq!(
        unfiltered
            .items
            .iter()
            .map(|key| key.id)
            .collect::<Vec<_>>(),
        ids[..2]
    );
    assert_eq!(unfiltered.next_cursor, Some(ids[1]));
    let remainder =
        olp::access::api_keys::repository::list_api_keys(&pool, None, unfiltered.next_cursor, 2)
            .await
            .unwrap();
    assert_eq!(
        remainder.items.iter().map(|key| key.id).collect::<Vec<_>>(),
        ids[2..]
    );
    assert!(remainder.next_cursor.is_none());
    let missing =
        olp::access::api_keys::repository::list_api_keys(&pool, Some(Uuid::now_v7()), None, 1)
            .await
            .unwrap();
    assert!(missing.items.is_empty());
    assert!(missing.next_cursor.is_none());
    pool.close().await;
    drop(db);
}
