pub(crate) fn valid_cost_limit(value: rust_decimal::Decimal) -> bool {
    value > rust_decimal::Decimal::ZERO
        && value.scale() <= 12
        && value < rust_decimal::Decimal::from(1_000_000_000_000_i64)
}

/// Truncates a query result fetched with `limit + 1` and derives the cursor
/// from the last visible item only when another page exists.
pub(crate) fn split_page<T, C>(
    mut items: Vec<T>,
    limit: usize,
    cursor: impl FnOnce(&T) -> C,
) -> (Vec<T>, Option<C>) {
    let has_more = items.len() > limit;
    items.truncate(limit);
    let next_cursor = if has_more {
        items.last().map(cursor)
    } else {
        None
    };
    (items, next_cursor)
}

#[cfg(test)]
mod tests {

    use rust_decimal::Decimal;

    use crate::database::query::split_page;

    #[test]
    pub(crate) fn split_page_distinguishes_complete_and_overfetched_results() {
        assert_eq!(split_page(vec![1, 2], 3, |item| *item), (vec![1, 2], None));
        assert_eq!(split_page(vec![1, 2], 2, |item| *item), (vec![1, 2], None));
        assert_eq!(
            split_page(vec![1, 2, 3], 2, |item| *item),
            (vec![1, 2], Some(2))
        );
    }

    #[test]
    pub(crate) fn split_page_never_derives_a_cursor_without_a_visible_item() {
        assert_eq!(split_page(vec![1], 0, |item| *item), (Vec::new(), None));
    }

    #[test]
    fn cost_limits_match_numeric_24_12_without_database_rounding() {
        assert!(crate::database::query::valid_cost_limit(Decimal::new(
            1, 12
        )));
        assert!(crate::database::query::valid_cost_limit(Decimal::new(
            999_999_999_999_999_999,
            6
        )));
        assert!(!crate::database::query::valid_cost_limit(Decimal::ZERO));
        assert!(!crate::database::query::valid_cost_limit(Decimal::new(
            1, 13
        )));
        assert!(!crate::database::query::valid_cost_limit(Decimal::from(
            1_000_000_000_000_i64
        )));
    }
}
