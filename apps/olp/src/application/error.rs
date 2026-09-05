pub(crate) type AppError = Box<dyn std::error::Error + Send + Sync>;
pub(crate) type AppResult<T> = Result<T, AppError>;
