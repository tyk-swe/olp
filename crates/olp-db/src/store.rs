use std::{net::IpAddr, time::Duration};

use sqlx::{PgPool, migrate::Migrate as _, postgres::PgPoolOptions};

use crate::error::Error;

/// Concrete PostgreSQL handle shared by storage subsystems.
///
/// Feature-specific queries are implemented in their owning modules; this
/// module owns only pool construction, access, migration, and liveness.
/// Boundary attribution recorded on the audit rows a request produces. The
/// full user-agent string is never stored; only its leading product token.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct RequestProvenance {
    pub source_ip: Option<IpAddr>,
    pub user_agent_family: Option<String>,
}

impl RequestProvenance {
    pub(crate) fn source_ip_text(&self) -> Option<String> {
        self.source_ip.map(|address| address.to_string())
    }

    pub(crate) fn user_agent_family(&self) -> Option<&str> {
        self.user_agent_family.as_deref()
    }
}

#[derive(Clone)]
pub struct Store {
    pool: PgPool,
    provenance: RequestProvenance,
}

impl Store {
    pub async fn connect(database_url: &str, max_connections: u32) -> Result<Self, Error> {
        let pool = PgPoolOptions::new()
            .max_connections(max_connections)
            .acquire_timeout(Duration::from_secs(5))
            // Usage buckets are UTC hours, and `date_trunc` on a timestamptz
            // truncates in the *session* TimeZone: a server or role defaulting
            // to a half-hour offset would write buckets the UTC-derived readers
            // can never match. SQLx sends TimeZone=UTC in its startup packet
            // today, but that is the driver's choice, not ours. Restate it so
            // the invariant survives a driver default change and holds for any
            // connection this pool hands out, whatever TZ/PGTZ the deployment
            // sets. The SQL states UTC explicitly as well, for readers that do
            // not come through this pool at all.
            .after_connect(|connection, _| {
                Box::pin(async move {
                    sqlx::raw_sql("SET TimeZone = 'UTC'")
                        .execute(connection)
                        .await?;
                    Ok(())
                })
            })
            .connect(database_url)
            .await?;
        Ok(Self {
            pool,
            provenance: RequestProvenance::default(),
        })
    }

    #[must_use]
    pub fn from_pool(pool: PgPool) -> Self {
        Self {
            pool,
            provenance: RequestProvenance::default(),
        }
    }

    /// Returns a handle whose audit writes carry the request boundary's
    /// attribution. Handles that never receive one - workers, maintenance, and
    /// reconciliation - write audit rows without it.
    #[must_use]
    pub fn with_provenance(&self, provenance: &RequestProvenance) -> Self {
        Self {
            pool: self.pool.clone(),
            provenance: provenance.clone(),
        }
    }

    #[must_use]
    pub(crate) fn provenance(&self) -> &RequestProvenance {
        &self.provenance
    }

    #[must_use]
    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    pub async fn migrate(&self) -> Result<(), Error> {
        self.run_migrations(None).await
    }

    pub async fn migrate_to(&self, target: i64) -> Result<(), Error> {
        self.run_migrations(Some(target)).await
    }

    async fn run_migrations(&self, target: Option<i64>) -> Result<(), Error> {
        let mut connection = self.pool.acquire().await?;
        connection.close_on_drop();

        // Keep SQLx's reentrant migration lock held across stale-index cleanup
        // and migration 34. Closing the session releases it on cancellation.
        connection.lock().await?;
        if target.is_none_or(|target| target >= 34) {
            connection
                .ensure_migrations_table("_sqlx_migrations")
                .await?;
            let applied = connection
                .list_applied_migrations("_sqlx_migrations")
                .await?;
            if !applied.iter().any(|migration| migration.version == 34) {
                sqlx::raw_sql("DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_facts_event_id_idx")
                    .execute(&mut *connection)
                    .await?;
            }
        }

        if let Some(target) = target {
            crate::MIGRATOR.run_to(target, &mut *connection).await?;
        } else {
            crate::MIGRATOR.run(&mut *connection).await?;
        }
        connection.unlock().await?;
        Ok(())
    }

    pub async fn ping(&self) -> Result<(), Error> {
        sqlx::query_scalar!("SELECT 1::int AS \"value!\"")
            .fetch_one(&self.pool)
            .await?;
        Ok(())
    }
}
