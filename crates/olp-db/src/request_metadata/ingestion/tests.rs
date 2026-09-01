use super::*;

#[test]
fn charge_status_and_optional_sums_cover_closed_boundaries() {
    assert_eq!(AttemptChargeStatus::NotBillable.as_str(), "not_billable");
    assert_eq!(AttemptChargeStatus::Billable.as_str(), "billable");
    assert_eq!(
        AttemptChargeStatus::BillingUncertain.as_str(),
        "billing_uncertain"
    );

    let integers = [None, Some(2_i64), None, Some(3)];
    let integer_refs = integers.iter().collect::<Vec<_>>();
    assert_eq!(
        checked_optional_i64_sum(&integer_refs, |value| *value).unwrap(),
        Some(5)
    );
    assert_eq!(
        checked_optional_i64_sum(&[&None::<i64>], |value| *value).unwrap(),
        None
    );
    assert!(checked_optional_i64_sum(&[&Some(i64::MAX), &Some(1)], |value| *value).is_err());

    let decimals = [None, Some(Decimal::ONE), Some(Decimal::new(25, 1))];
    let decimal_refs = decimals.iter().collect::<Vec<_>>();
    assert_eq!(
        checked_optional_decimal_sum(&decimal_refs, |value| *value).unwrap(),
        Some(Decimal::new(35, 1))
    );
    assert!(
        checked_optional_decimal_sum(&[&Some(Decimal::MAX), &Some(Decimal::ONE)], |value| {
            *value
        })
        .is_err()
    );
}
