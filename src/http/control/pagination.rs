use serde::Deserialize;
use utoipa::IntoParams;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::database::reads::MAX_PAGE_SIZE;
use crate::http::problem::Problem;

/// Page size used when the caller does not ask for one.
pub(crate) const DEFAULT_PAGE_SIZE: u16 = 50;

#[derive(Debug, Deserialize, IntoParams, ToSchema)]
#[into_params(parameter_in = Query)]
pub(crate) struct PageQuery {
    /// Opaque cursor returned by the previous page.
    pub cursor: Option<String>,
    /// Page size, from 1 to 200. Defaults to 50.
    #[param(minimum = 1, maximum = 200)]
    pub limit: Option<u16>,
}

#[derive(Debug, Deserialize)]
pub(crate) struct DiffQuery {
    pub from: Uuid,
    pub to: Uuid,
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
    Ok((cursor, i64::from(page_limit(query.limit)?)))
}

pub(crate) fn page_limit(value: Option<u16>) -> Result<u16, Problem> {
    let value = value.unwrap_or(DEFAULT_PAGE_SIZE);
    if (1..=MAX_PAGE_SIZE).contains(&value) {
        return Ok(value);
    }
    Err(Problem::bad_request(
        "invalid_page_size",
        format!("Page size must be between 1 and {MAX_PAGE_SIZE}."),
    ))
}

#[cfg(test)]
mod tests {
    use crate::database::reads::MAX_PAGE_SIZE;
    use crate::http::control::pagination::DEFAULT_PAGE_SIZE;
    use crate::http::control::pagination::PageQuery;
    use crate::http::control::pagination::page;
    use crate::http::control::pagination::page_limit;

    #[test]
    fn an_absent_limit_uses_the_default_page_size() {
        assert_eq!(page_limit(None).unwrap(), DEFAULT_PAGE_SIZE);
    }

    #[test]
    fn every_collection_shares_one_limit_range_and_one_status() {
        assert_eq!(page_limit(Some(1)).unwrap(), 1);
        assert_eq!(page_limit(Some(MAX_PAGE_SIZE)).unwrap(), MAX_PAGE_SIZE);
        for rejected in [0, MAX_PAGE_SIZE + 1] {
            let problem = page_limit(Some(rejected)).unwrap_err();
            assert_eq!(problem.status, 400);
            assert_eq!(
                problem.detail.as_ref(),
                "Page size must be between 1 and 200."
            );
        }
    }

    #[test]
    fn the_keyset_page_helper_applies_the_same_bounds() {
        let (cursor, limit) = page(PageQuery {
            cursor: None,
            limit: Some(200),
        })
        .unwrap();
        assert!(cursor.is_none());
        assert_eq!(limit, 200);

        let problem = page(PageQuery {
            cursor: None,
            limit: Some(201),
        })
        .unwrap_err();
        assert_eq!(problem.status, 400);

        let problem = page(PageQuery {
            cursor: Some("not-a-uuid".to_owned()),
            limit: None,
        })
        .unwrap_err();
        assert_eq!(problem.status, 400);
    }
}
