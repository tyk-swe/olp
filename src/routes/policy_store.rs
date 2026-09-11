use crate::routes::policy::*;
use sqlx::{Postgres, Transaction};

pub async fn installation_policy(
    transaction: &mut Transaction<'_, Postgres>,
) -> Result<RoutingPolicy, sqlx::Error> {
    Ok(sqlx::query_scalar::<_, sqlx::types::Json<RoutingPolicy>>(
        "SELECT policy FROM routing_settings WHERE singleton",
    )
    .fetch_one(&mut **transaction)
    .await?
    .0)
}

/// The latest revision's policy for every route, keyed by slug.
pub async fn route_policies(
    transaction: &mut Transaction<'_, Postgres>,
) -> Result<std::collections::BTreeMap<crate::ids::RouteSlug, RoutingPolicy>, sqlx::Error> {
    let rows = sqlx::query_as::<_, (String, sqlx::types::Json<RoutingPolicy>)>(
        "SELECT DISTINCT ON (route_id) slug, routing_policy FROM route_revisions ORDER BY \
            route_id, revision DESC",
    )
    .fetch_all(&mut **transaction)
    .await?;
    rows.into_iter()
        .map(|(slug, policy)| {
            crate::ids::RouteSlug::parse(slug)
                .map(|slug| (slug, policy.0))
                .map_err(|e| sqlx::Error::Protocol(e.to_string()))
        })
        .collect()
}

pub async fn load(
    transaction: &mut Transaction<'_, Postgres>,
) -> Result<RoutingConfiguration, sqlx::Error> {
    let mut result = RoutingConfiguration {
        version: 1,
        installation: installation_policy(transaction).await?,
        routes: route_policies(transaction).await?,
        ..Default::default()
    };
    let rows = sqlx::query_as::<
        _,
        (
            uuid::Uuid,
            sqlx::types::Json<crate::providers::options::ConnectionOptions>,
            String,
            Option<String>,
        ),
    >(
        "SELECT p.id, pr.options, pr.kind, pr.endpoint FROM providers p JOIN provider_revisions \
            pr ON pr.id = p.active_revision_id WHERE p.state <> 'disabled'",
    )
    .fetch_all(&mut **transaction)
    .await?;
    for (id, mut options, kind, endpoint) in rows {
        if let Ok(kind) = kind.parse() {
            options.vendor_id = crate::providers::catalog::effective_vendor(
                options.vendor_id.as_deref(),
                kind,
                endpoint.as_deref(),
            )
            .map(str::to_owned);
        }
        result
            .providers
            .insert(crate::ids::ProviderId::from_uuid(id), options.0);
    }
    result.credentials = credential_slots(transaction).await?;
    let rows = sqlx::query_as::<_, (uuid::Uuid, uuid::Uuid, String, String, String, Option<String>, Option<String>, Option<String>, chrono::DateTime<chrono::Utc>, i32, i32, Option<String>)>(
        "WITH matched AS (
          SELECT revision.id AS revision_id,p.id AS provider_id,price.model,price.operation,price.currency::text AS currency,
                 price.input_per_million::text AS input_per_million,price.output_per_million::text AS output_per_million,price.unit_price::text AS unit_price,
                 revision.effective_at,revision.revision,
                 (CASE WHEN price.provider_id IS NOT NULL THEN 2 ELSE 0 END + CASE WHEN price.vendor_id IS NOT NULL THEN 1 ELSE 0 END) AS scope_priority,
                 price.vendor_id,price.provider_id AS connection_scope
          FROM providers p JOIN provider_revisions pr ON pr.id=p.active_revision_id
          JOIN provider_revision_models pm ON pm.provider_revision_id=pr.id AND pm.enabled
          JOIN prices price ON price.provider_kind=pr.kind AND price.model=pm.upstream_model AND (price.provider_id IS NULL OR price.provider_id=p.id)
          JOIN pricing_revisions revision ON revision.id=price.pricing_revision_id
        ), current_prices AS (
          SELECT DISTINCT ON(provider_id,model,operation,vendor_id,connection_scope) * FROM matched WHERE effective_at<=now()
          ORDER BY provider_id,model,operation,vendor_id,connection_scope,effective_at DESC,revision DESC
        ) SELECT revision_id,provider_id,model,operation,currency,input_per_million,output_per_million,unit_price,effective_at,revision,scope_priority,vendor_id FROM current_prices
          UNION ALL SELECT revision_id,provider_id,model,operation,currency,input_per_million,output_per_million,unit_price,effective_at,revision,scope_priority,vendor_id FROM matched WHERE effective_at>now()"

    ).fetch_all(&mut **transaction).await?;
    for (
        revision_id,
        provider_id,
        model,
        operation,
        currency,
        input_per_million,
        output_per_million,
        unit_price,
        effective_at,
        revision,
        scope_priority,
        vendor_id,
    ) in rows
    {
        if vendor_id.as_deref().is_some_and(|vendor| {
            result
                .providers
                .get(&crate::ids::ProviderId::from_uuid(provider_id))
                .and_then(|o| o.vendor_id.as_deref())
                != Some(vendor)
        }) {
            continue;
        }
        result.prices.push(RoutingPrice {
            effective_at,
            revision,
            scope_priority: scope_priority as u8,
            revision_id,
            provider_id,
            model,
            operation,
            currency,
            input_per_million,
            output_per_million,
            unit_price,
        });
    }
    Ok(result)
}

/// Current published connection quotas, excluding draft-only changes.
pub async fn connection_limits(
    transaction: &mut Transaction<'_, Postgres>,
) -> Result<
    std::collections::BTreeMap<crate::ids::ProviderId, crate::providers::options::ConnectionLimits>,
    sqlx::Error,
> {
    let rows = sqlx::query_as::<_, (uuid::Uuid, sqlx::types::Json<Option<crate::providers::options::ConnectionLimits>>)>(
        "SELECT p.id,COALESCE(pr.options->'limits','null'::jsonb) FROM providers p JOIN provider_revisions pr ON pr.id=p.active_revision_id",
    ).fetch_all(&mut **transaction).await?;
    Ok(rows
        .into_iter()
        .map(|(id, limits)| {
            (
                crate::ids::ProviderId::from_uuid(id),
                limits.0.unwrap_or_default(),
            )
        })
        .collect())
}

/// Current published credential slots, excluding draft-only changes.
pub async fn credential_slots(
    transaction: &mut Transaction<'_, Postgres>,
) -> Result<
    std::collections::BTreeMap<crate::ids::ProviderId, Vec<crate::providers::pool::CredentialSlot>>,
    sqlx::Error,
> {
    let mut result = std::collections::BTreeMap::new();
    let providers: Vec<uuid::Uuid> = sqlx::query_scalar(
        "SELECT p.id FROM providers p JOIN provider_revisions pr ON pr.id=p.active_revision_id",
    )
    .fetch_all(&mut **transaction)
    .await?;
    for id in providers {
        result.insert(crate::ids::ProviderId::from_uuid(id), Vec::new());
    }
    let rows = sqlx::query_as::<
        _,
        (
            uuid::Uuid,
            sqlx::types::Json<crate::providers::pool::CredentialSlot>,
        ),
    >(
        "SELECT p.id, pc.configuration FROM providers p JOIN provider_revision_credentials pc ON \
            pc.provider_revision_id = p.active_revision_id WHERE p.state <> 'disabled' ORDER BY \
            p.id, pc.slot_id",
    )
    .fetch_all(&mut **transaction)
    .await?;
    for (id, slot) in rows {
        result
            .entry(crate::ids::ProviderId::from_uuid(id))
            .or_default()
            .push(slot.0);
    }
    Ok(result)
}
