use super::error::AppResult;
use std::path::Path;
use zeroize::Zeroizing;
pub(crate) async fn read_secret_file(path: &Path) -> AppResult<Zeroizing<String>> {
    Ok(Zeroizing::new(tokio::fs::read_to_string(path).await?))
}

#[cfg(unix)]
pub(crate) async fn check_secret_permissions(path: &Path) -> AppResult<()> {
    use std::os::unix::fs::PermissionsExt;
    let mode = tokio::fs::metadata(path).await?.permissions().mode() & 0o777;
    if mode & 0o007 != 0 {
        return Err(std::io::Error::other(format!(
            "{} must not be accessible by other users",
            path.display()
        ))
        .into());
    }
    Ok(())
}

#[cfg(not(unix))]
pub(crate) async fn check_secret_permissions(path: &Path) -> AppResult<()> {
    tokio::fs::metadata(path).await?;
    Ok(())
}
