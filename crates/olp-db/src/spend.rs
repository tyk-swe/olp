use std::{collections::BTreeMap, str::FromStr as _};

use chrono::{DateTime, Datelike as _, NaiveDate, Utc};
use rust_decimal::Decimal;
use sqlx::{Postgres, Transaction};
use uuid::Uuid;

use crate::{error::Error, limits::CostSnapshot, store::Store};

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BudgetWindowStatus {
    pub accrued: Decimal,
    pub window_ends_at: DateTime<Utc>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ApiKeyBudgetStatus {
    pub daily: BudgetWindowStatus,
    pub monthly: BudgetWindowStatus,
    pub unpriced_attempts: u64,
}

struct BudgetWindows {
    daily_start: DateTime<Utc>,
    daily_end: DateTime<Utc>,
    monthly_start: DateTime<Utc>,
    monthly_end: DateTime<Utc>,
    daily_id: i64,
    monthly_id: i64,
}

struct CostCounterRow {
    api_key_id: Uuid,
    daily_window_id: i64,
    daily_accrued: String,
    monthly_window_id: i64,
    monthly_accrued: String,
    monthly_unpriced_attempts: i64,
}

impl Store {
    pub async fn api_key_budget_status(
        &self,
        api_key_id: Uuid,
        now: DateTime<Utc>,
    ) -> Result<ApiKeyBudgetStatus, Error> {
        self.api_key_budget_statuses(&[api_key_id], now)
            .await?
            .remove(&api_key_id)
            .ok_or(Error::InvalidStoredValue("API key budget status"))
    }

    pub async fn api_key_budget_statuses(
        &self,
        api_key_ids: &[Uuid],
        now: DateTime<Utc>,
    ) -> Result<BTreeMap<Uuid, ApiKeyBudgetStatus>, Error> {
        if api_key_ids.is_empty() {
            return Ok(BTreeMap::new());
        }
        let windows = budget_windows(now)?;
        let rows = sqlx::query!(
            "WITH usage AS ( \
               SELECT api_key_id, observed_at, COALESCE(estimated_cost, 0)::numeric AS cost, \
                      CASE WHEN charge_status <> 'not_billable' AND unpriced \
                           THEN 1 ELSE 0 END::bigint AS unpriced_attempts \
               FROM attempt_usage_facts \
               WHERE api_key_id = ANY($1::uuid[]) AND observed_at >= $3 AND observed_at < $4 \
               UNION ALL \
               SELECT api_key_id, bucket AS observed_at, \
                      COALESCE(estimated_cost, 0)::numeric AS cost, \
                      unpriced_attempt_count AS unpriced_attempts \
               FROM attempt_usage_hourly \
               WHERE api_key_id = ANY($1::uuid[]) AND bucket >= $3 AND bucket < $4 \
             ), requested AS (SELECT DISTINCT unnest($1::uuid[]) AS api_key_id) \
             SELECT requested.api_key_id AS \"api_key_id!\", \
                    COALESCE(SUM(usage.cost) FILTER (WHERE usage.observed_at >= $2), 0)::text \
                      AS \"daily_accrued!\", \
                    COALESCE(SUM(usage.cost), 0)::text AS \"monthly_accrued!\", \
                    COALESCE(SUM(usage.unpriced_attempts), 0)::bigint \
                      AS \"unpriced_attempts!\" \
             FROM requested LEFT JOIN usage ON usage.api_key_id = requested.api_key_id \
             GROUP BY requested.api_key_id",
            api_key_ids,
            windows.daily_start,
            windows.monthly_start,
            windows.monthly_end,
        )
        .fetch_all(self.pool())
        .await?;
        rows.into_iter()
            .map(|row| {
                let status = ApiKeyBudgetStatus {
                    daily: BudgetWindowStatus {
                        accrued: parse_cost(&row.daily_accrued)?,
                        window_ends_at: windows.daily_end,
                    },
                    monthly: BudgetWindowStatus {
                        accrued: parse_cost(&row.monthly_accrued)?,
                        window_ends_at: windows.monthly_end,
                    },
                    unpriced_attempts: checked_unpriced(row.unpriced_attempts)?,
                };
                Ok((row.api_key_id, status))
            })
            .collect()
    }

    pub(crate) async fn cost_reconciliation_snapshots(
        &self,
        now: DateTime<Utc>,
    ) -> Result<Vec<CostSnapshot>, Error> {
        let windows = budget_windows(now)?;
        let rows = sqlx::query_as!(
            CostCounterRow,
            "WITH active_keys AS ( \
               SELECT id AS api_key_id FROM api_keys \
               WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $1) \
             ), usage AS ( \
               SELECT fact.api_key_id, fact.observed_at, \
                      COALESCE(fact.estimated_cost, 0)::numeric AS cost, \
                      CASE WHEN fact.charge_status <> 'not_billable' AND fact.unpriced \
                           THEN 1 ELSE 0 END::bigint AS unpriced_attempts \
               FROM attempt_usage_facts fact JOIN active_keys key \
                 ON key.api_key_id = fact.api_key_id \
               WHERE fact.observed_at >= $3 AND fact.observed_at < $4 \
               UNION ALL \
               SELECT hourly.api_key_id, hourly.bucket, \
                      COALESCE(hourly.estimated_cost, 0)::numeric, \
                      hourly.unpriced_attempt_count \
               FROM attempt_usage_hourly hourly JOIN active_keys key \
                 ON key.api_key_id = hourly.api_key_id \
               WHERE hourly.bucket >= $3 AND hourly.bucket < $4 \
             ), totals AS ( \
               SELECT key.api_key_id, \
                      COALESCE(SUM(usage.cost) FILTER (WHERE usage.observed_at >= $2), 0) \
                        AS daily_accrued, \
                      COALESCE(SUM(usage.cost), 0) AS monthly_accrued, \
                      COALESCE(SUM(usage.unpriced_attempts), 0)::bigint AS unpriced_attempts \
               FROM active_keys key LEFT JOIN usage ON usage.api_key_id = key.api_key_id \
               GROUP BY key.api_key_id \
             ), pruned AS ( \
               DELETE FROM api_key_cost_windows \
               WHERE (window_kind = 'day' AND window_id < $5) \
                  OR (window_kind = 'month' AND window_id < $6) RETURNING 1 \
             ), desired AS ( \
               SELECT api_key_id, 'day'::text AS window_kind, $5 AS window_id, \
                      daily_accrued AS accrued, 0::bigint AS unpriced_attempts FROM totals \
               UNION ALL \
               SELECT api_key_id, 'month', $6, monthly_accrued, unpriced_attempts FROM totals \
             ), reconciled AS ( \
               INSERT INTO api_key_cost_windows \
                 (api_key_id, window_kind, window_id, accrued, unpriced_attempts) \
               SELECT api_key_id, window_kind, window_id, accrued, unpriced_attempts \
               FROM desired \
               ON CONFLICT (api_key_id, window_kind, window_id) DO UPDATE SET \
                 accrued = GREATEST(api_key_cost_windows.accrued, EXCLUDED.accrued), \
                 unpriced_attempts = GREATEST(api_key_cost_windows.unpriced_attempts, \
                                              EXCLUDED.unpriced_attempts) \
               RETURNING api_key_id, window_kind, window_id, accrued, unpriced_attempts \
             ) SELECT api_key_id, $5 AS \"daily_window_id!\", \
                      MAX(accrued) FILTER (WHERE window_kind = 'day')::text \
                        AS \"daily_accrued!\", \
                      $6 AS \"monthly_window_id!\", \
                      MAX(accrued) FILTER (WHERE window_kind = 'month')::text \
                        AS \"monthly_accrued!\", \
                      MAX(unpriced_attempts) FILTER (WHERE window_kind = 'month')::bigint \
                        AS \"monthly_unpriced_attempts!\" \
               FROM reconciled GROUP BY api_key_id ORDER BY api_key_id",
            now,
            windows.daily_start,
            windows.monthly_start,
            windows.monthly_end,
            windows.daily_id,
            windows.monthly_id,
        )
        .fetch_all(self.pool())
        .await?;
        rows.into_iter().map(snapshot_from_row).collect()
    }
}

#[cfg(any(test, feature = "test-util"))]
impl Store {
    #[doc(hidden)]
    pub async fn add_cost_delta_for_test(
        &self,
        api_key_id: Uuid,
        observed_at: DateTime<Utc>,
        cost: Decimal,
        unpriced_attempts: u64,
    ) -> Result<CostSnapshot, Error> {
        let mut transaction = self.pool().begin().await?;
        let snapshot = add_cost_delta_on(
            &mut transaction,
            api_key_id,
            observed_at,
            cost,
            unpriced_attempts,
        )
        .await?;
        transaction.commit().await?;
        Ok(snapshot)
    }
}

pub(crate) async fn add_cost_delta_on(
    transaction: &mut Transaction<'_, Postgres>,
    api_key_id: Uuid,
    observed_at: DateTime<Utc>,
    cost: Decimal,
    unpriced_attempts: u64,
) -> Result<CostSnapshot, Error> {
    let windows = budget_windows(observed_at)?;
    let unpriced_attempts = i64::try_from(unpriced_attempts)
        .map_err(|_| Error::InvalidStoredValue("unpriced attempt count"))?;
    let row = sqlx::query_as!(
        CostCounterRow,
        "WITH deltas (window_kind, window_id, accrued, unpriced_attempts) AS ( \
           VALUES ('day'::text, $2::bigint, $3::numeric, 0::bigint), \
                  ('month'::text, $4::bigint, $3::numeric, $5::bigint) \
         ), applied AS ( \
           INSERT INTO api_key_cost_windows \
             (api_key_id, window_kind, window_id, accrued, unpriced_attempts) \
           SELECT $1, window_kind, window_id, accrued, unpriced_attempts FROM deltas \
           ON CONFLICT (api_key_id, window_kind, window_id) DO UPDATE SET \
             accrued = api_key_cost_windows.accrued + EXCLUDED.accrued, \
             unpriced_attempts = api_key_cost_windows.unpriced_attempts \
                                 + EXCLUDED.unpriced_attempts \
           RETURNING api_key_id, window_kind, window_id, accrued, unpriced_attempts \
         ) SELECT api_key_id, \
                    MAX(window_id) FILTER (WHERE window_kind = 'day')::bigint \
                      AS \"daily_window_id!\", \
                    MAX(accrued) FILTER (WHERE window_kind = 'day')::text \
                      AS \"daily_accrued!\", \
                    MAX(window_id) FILTER (WHERE window_kind = 'month')::bigint \
                      AS \"monthly_window_id!\", \
                    MAX(accrued) FILTER (WHERE window_kind = 'month')::text \
                      AS \"monthly_accrued!\", \
                    MAX(unpriced_attempts) FILTER (WHERE window_kind = 'month')::bigint \
                      AS \"monthly_unpriced_attempts!\" \
             FROM applied GROUP BY api_key_id",
        api_key_id,
        windows.daily_id,
        cost,
        windows.monthly_id,
        unpriced_attempts,
    )
    .fetch_one(&mut **transaction)
    .await?;
    snapshot_from_row(row)
}

fn snapshot_from_row(row: CostCounterRow) -> Result<CostSnapshot, Error> {
    Ok(CostSnapshot {
        api_key_id: row.api_key_id,
        daily_window_id: row.daily_window_id,
        daily_accrued: parse_cost(&row.daily_accrued)?,
        monthly_window_id: row.monthly_window_id,
        monthly_accrued: parse_cost(&row.monthly_accrued)?,
        unpriced_attempts: checked_unpriced(row.monthly_unpriced_attempts)?,
    })
}

fn budget_windows(now: DateTime<Utc>) -> Result<BudgetWindows, Error> {
    let date = now.date_naive();
    let daily_start = utc_midnight(date)?;
    let daily_end = utc_midnight(
        date.succ_opt()
            .ok_or(Error::InvalidStoredValue("budget window"))?,
    )?;
    let monthly_start = utc_midnight(
        NaiveDate::from_ymd_opt(date.year(), date.month(), 1)
            .ok_or(Error::InvalidStoredValue("budget window"))?,
    )?;
    let (next_year, next_month) = if date.month() == 12 {
        (date.year() + 1, 1)
    } else {
        (date.year(), date.month() + 1)
    };
    let monthly_end = utc_midnight(
        NaiveDate::from_ymd_opt(next_year, next_month, 1)
            .ok_or(Error::InvalidStoredValue("budget window"))?,
    )?;
    let daily_id = daily_start.timestamp().div_euclid(86_400);
    if daily_id < 0 {
        return Err(Error::InvalidStoredValue("budget window"));
    }
    Ok(BudgetWindows {
        daily_start,
        daily_end,
        monthly_start,
        monthly_end,
        daily_id,
        monthly_id: i64::from(date.year()) * 12 + i64::from(date.month0()),
    })
}

fn utc_midnight(date: NaiveDate) -> Result<DateTime<Utc>, Error> {
    date.and_hms_opt(0, 0, 0)
        .map(|value| value.and_utc())
        .ok_or(Error::InvalidStoredValue("budget window"))
}

fn parse_cost(value: &str) -> Result<Decimal, Error> {
    Decimal::from_str(value).map_err(|_| Error::InvalidStoredValue("budget cost"))
}

fn checked_unpriced(value: i64) -> Result<u64, Error> {
    u64::try_from(value).map_err(|_| Error::InvalidStoredValue("unpriced attempt count"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone as _;

    #[test]
    fn windows_end_at_fixed_utc_day_and_month_boundaries() {
        let windows =
            budget_windows(Utc.with_ymd_and_hms(2028, 2, 29, 23, 59, 59).unwrap()).unwrap();
        assert_eq!(
            windows.daily_end,
            Utc.with_ymd_and_hms(2028, 3, 1, 0, 0, 0).unwrap()
        );
        assert_eq!(windows.daily_end, windows.monthly_end);
        assert_eq!(windows.monthly_id, 2028 * 12 + 1);
    }
}
