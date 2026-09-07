use crate::usage::ingestion::persistence::*;

#[test]
fn charge_status_uses_closed_storage_values() {
    assert_eq!(AttemptChargeStatus::NotBillable.as_str(), "not_billable");
    assert_eq!(AttemptChargeStatus::Billable.as_str(), "billable");
    assert_eq!(
        AttemptChargeStatus::BillingUncertain.as_str(),
        "billing_uncertain"
    );
}
