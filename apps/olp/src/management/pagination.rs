use serde::Deserialize;
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use crate::Problem;

#[derive(Debug, Deserialize, IntoParams, ToSchema)]
pub(crate) struct PageQuery {
    pub cursor: Option<String>,
    pub limit: Option<u16>,
}

pub(crate) fn page(query: PageQuery) -> Result<(Option<Uuid>, i64), Problem> {
    let cursor = query
        .cursor
        .map(|cursor| {
            Uuid::parse_str(&cursor).map_err(|_| {
                Problem::bad_request(
                    "invalid_cursor",
                    "The pagination cursor is invalid or malformed.",
                )
            })
        })
        .transpose()?;
    let limit = query.limit.unwrap_or(50);
    if !(1..=100).contains(&limit) {
        return Err(Problem::bad_request(
            "invalid_page_size",
            "Page size must be between 1 and 100.",
        ));
    }
    Ok((cursor, i64::from(limit)))
}
