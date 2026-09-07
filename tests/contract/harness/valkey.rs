use rand::Rng as _;
use sqlx::Connection as _;

pub(super) async fn valkey(admin_url: &str) -> Result<(String, Option<ValkeyReservation>), String> {
    if let Ok(url) = std::env::var("OLP_E2E_VALKEY_URL") {
        return Ok((std::env::var("OLP_E2E_VALKEY_APP_URL").unwrap_or(url), None));
    }

    let mut byte = [0_u8; 1];
    rand::rng().fill_bytes(&mut byte);
    let start = usize::from(byte[0]) % 15;
    for offset in 0..15 {
        let database = u16::try_from(1 + (start + offset) % 15).unwrap();
        let mut lock = sqlx::postgres::PgConnection::connect(admin_url)
            .await
            .map_err(|error| format!("failed to connect for a Valkey reservation: {error}"))?;
        let acquired: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1, $2)")
            .bind(0x4f4c_5002_i32)
            .bind(i32::from(database))
            .fetch_one(&mut lock)
            .await
            .map_err(|error| format!("failed to reserve a Valkey logical database: {error}"))?;
        if !acquired {
            lock.close().await.ok();
            continue;
        }
        if let Err(error) = flush_valkey(database).await {
            lock.close().await.ok();
            return Err(error);
        }
        let reservation = ValkeyReservation { database, lock };
        return Ok((reservation.url(), Some(reservation)));
    }
    Err("all 15 local Valkey logical databases are reserved by other E2E runs".to_owned())
}

pub(super) struct ValkeyReservation {
    database: u16,
    lock: sqlx::postgres::PgConnection,
}

/// Owns one test Valkey lease independently of any OLP installation. Multiple
/// independently migrated servers can therefore share the exact URL while the
/// advisory lease remains held until every server has stopped.
pub(crate) struct SharedValkey {
    url: String,
    reservation: Option<ValkeyReservation>,
}

impl SharedValkey {
    pub(crate) async fn reserve() -> Result<Self, String> {
        let (url, reservation) = valkey(&super::admin_url()).await?;
        Ok(Self { url, reservation })
    }

    pub(crate) fn url(&self) -> &str {
        &self.url
    }

    pub(crate) async fn release(mut self) {
        if let Some(reservation) = self.reservation.take() {
            reservation.release().await;
        }
    }
}

impl ValkeyReservation {
    fn url(&self) -> String {
        format!("redis://localhost:6379/{}", self.database)
    }

    pub(super) async fn release(self) {
        flush_valkey(self.database).await.ok();
        self.lock.close().await.ok();
    }
}

async fn flush_valkey(database: u16) -> Result<(), String> {
    let client = redis::Client::open(format!("redis://localhost:6379/{database}"))
        .map_err(|error| format!("invalid local Valkey URL: {error}"))?;
    let mut connection = client
        .get_multiplexed_async_connection()
        .await
        .map_err(|error| format!("failed to connect to local Valkey: {error}"))?;
    let _: () = redis::cmd("FLUSHDB")
        .query_async(&mut connection)
        .await
        .map_err(|error| format!("failed to clear local Valkey database {database}: {error}"))?;
    Ok(())
}
