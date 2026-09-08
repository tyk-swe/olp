use std::{net::IpAddr, time::Duration};

use sqlx::{PgPool, migrate::Migrate as _, postgres::PgPoolOptions};

use crate::database::error::Error;

pub mod cursor;
#[cfg(test)]
mod cursor_tests;
pub mod error;
pub mod idempotency;
pub(crate) mod query;
pub(crate) mod reads;

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

pub async fn connect(database_url: &str, max_connections: u32) -> Result<PgPool, Error> {
    let pool = PgPoolOptions::new()
        .max_connections(max_connections)
        .acquire_timeout(Duration::from_secs(5))
        .after_connect(|connection, _| {
            Box::pin(async move {
                sqlx::raw_sql(
                    "SET TimeZone = 'UTC'; SET search_path = olp_v3, pg_catalog;
                    SELECT set_config(name, fallback, false)
                    FROM (VALUES ('statement_timeout', '30s'), ('lock_timeout', '5s'),
                        ('idle_in_transaction_session_timeout', '60s')) AS deadlines(name, fallback)
                    WHERE current_setting(name) = '0'",
                )
                .execute(connection)
                .await?;
                Ok(())
            })
        })
        .connect(database_url)
        .await?;
    refuse_legacy_storage(&mut *pool.acquire().await?).await?;
    Ok(pool)
}

async fn refuse_legacy_storage(connection: &mut sqlx::PgConnection) -> Result<(), Error> {
    let legacy = sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS (
           SELECT 1 FROM pg_catalog.pg_class relation
           JOIN pg_catalog.pg_namespace namespace ON namespace.oid = relation.relnamespace
           WHERE relation.relname IN ('_sqlx_migrations', 'installation_identity', 'installation')
             AND namespace.nspname <> 'olp_v3'
             AND namespace.nspname NOT IN ('pg_catalog', 'information_schema')
         )",
    )
    .fetch_one(connection)
    .await?;
    if legacy {
        return Err(Error::LegacyInstallation);
    }
    Ok(())
}

pub async fn migrate(pool: &PgPool) -> Result<(), Error> {
    let mut connection = pool.acquire().await?;
    connection.close_on_drop();
    sqlx::raw_sql("SET statement_timeout = '5min'; SET lock_timeout = '10s'")
        .execute(&mut *connection)
        .await?;
    connection.lock().await?;
    refuse_legacy_storage(&mut connection).await?;
    let mut migrator = sqlx::migrate!("./migrations");
    migrator.create_schema("olp_v3");
    migrator.dangerous_set_table_name("olp_v3._sqlx_migrations");
    migrator.run(&mut *connection).await?;
    connection.unlock().await?;
    Ok(())
}

pub async fn ping(pool: &PgPool) -> Result<(), Error> {
    sqlx::query_scalar::<_, i32>("SELECT 1")
        .fetch_one(pool)
        .await?;
    Ok(())
}

pub mod page;

#[cfg(test)]
mod tests;
