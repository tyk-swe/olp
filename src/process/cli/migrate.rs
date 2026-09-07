use crate::process::cli::{AppResult, config::DatabaseArgs, validation::connect_database};

pub(crate) async fn migrate(args: DatabaseArgs) -> AppResult<()> {
    let pool = connect_database(&args).await?;
    crate::database::migrate(&pool).await?;
    tracing::info!("PostgreSQL migrations are current");
    Ok(())
}
