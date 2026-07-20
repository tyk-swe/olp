use chrono::{DateTime, Utc};
use rust_decimal::Decimal;

use crate::PersistenceError;

pub(super) fn timestamp_millis(value: i64) -> Option<DateTime<Utc>> {
    (value > 0)
        .then(|| DateTime::<Utc>::from_timestamp_millis(value))
        .flatten()
}

pub(in crate::usage) fn usage_gap_count_from_decimal(
    value: Decimal,
) -> Result<u64, PersistenceError> {
    if !value.fract().is_zero() {
        return Err(PersistenceError::InvalidUsageGap);
    }
    u64::try_from(value).map_err(|_| PersistenceError::InvalidUsageGap)
}
