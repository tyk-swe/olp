use super::*;

pub(super) async fn assert_large_provider_collection(
    pool: &sqlx::PgPool,
    owner_id: Uuid,
    observability_state: &olp::observability::state::ObservabilityState,
    observability: &Router,
    cookie: &str,
) {
    for (additional, expected) in [(100, 101), (149, 250)] {
        let ids: Vec<Uuid> = (0..additional).map(|_| Uuid::now_v7()).collect();
        let names: Vec<String> = (0..additional)
            .map(|index| format!("metrics-{expected}-{index}"))
            .collect();
        let etags: Vec<Uuid> = (0..additional).map(|_| Uuid::now_v7()).collect();
        sqlx::query(
            "INSERT INTO providers (id, name, kind, state, auth_mode, etag, created_by)
            SELECT id, name, 'openai', 'draft', 'api_key', etag, $4
            FROM unnest($1::uuid[], $2::text[], $3::uuid[]) AS rows(id, name, etag)",
        )
        .bind(&ids)
        .bind(&names)
        .bind(&etags)
        .bind(owner_id)
        .execute(pool)
        .await
        .unwrap();
        refresh_observability_cache(observability_state).await;
        let response = get(observability, "/metrics", cookie).await;
        let body = String::from_utf8(
            response
                .into_body()
                .collect()
                .await
                .unwrap()
                .to_bytes()
                .to_vec(),
        )
        .unwrap();
        assert!(body.contains("olp_provider_metrics_complete 1\n"));
        assert!(body.contains(&format!("olp_provider_metrics_emitted {expected}\n")));
        assert_eq!(
            body.lines()
                .filter(|line| line.starts_with("olp_provider_health{"))
                .count(),
            expected
        );
        assert_eq!(
            body.lines()
                .filter(|line| line.starts_with("olp_provider_success_ratio_15m{"))
                .count(),
            0
        );
    }
}
