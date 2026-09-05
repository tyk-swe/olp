use super::{DAY_MS, FIXED_WINDOW_MS, MAX_LUA_INTEGER, MAX_MONTH_MS, SCRIPT_RESPONSE_VERSION};
use olp_engine::inference::limits::{LimitDimension, LimitError};
use redis::FromRedisValue;

pub(super) enum ReservationScriptResult {
    Granted {
        window_id: i64,
        concurrency_expires_at_ms: Option<i64>,
    },
    Rejected {
        dimension: LimitDimension,
        retry_after_ms: u64,
    },
    MalformedState,
    ScriptFailure,
}

pub(super) enum CostReservationScriptResult {
    Granted,
    UninitializedState,
    Rejected {
        dimension: LimitDimension,
        retry_after_ms: u64,
    },
    MalformedState,
    ScriptFailure,
}

impl CostReservationScriptResult {
    pub(super) fn parse_value(value: &redis::Value) -> Result<Self, LimitError> {
        let (version, status, detail, retry_after_ms, day_window, month_window) =
            RawReservationScriptResponse::from_redis_value_ref(value)
                .map_err(|_| LimitError::UnexpectedResponse)?;
        if version != SCRIPT_RESPONSE_VERSION || day_window <= 0 || month_window <= 0 {
            return Err(LimitError::UnexpectedResponse);
        }
        match (status, detail.as_str()) {
            (1, "ok") if retry_after_ms == 0 => Ok(Self::Granted),
            (0, "daily_cost") if (1..=DAY_MS).contains(&retry_after_ms) => Ok(Self::Rejected {
                dimension: LimitDimension::DailyCost,
                retry_after_ms: retry_after_ms as u64,
            }),
            (0, "monthly_cost") if (1..=MAX_MONTH_MS).contains(&retry_after_ms) => {
                Ok(Self::Rejected {
                    dimension: LimitDimension::MonthlyCost,
                    retry_after_ms: retry_after_ms as u64,
                })
            }
            (-1, "uninitialized_daily_cost_state" | "uninitialized_monthly_cost_state")
                if retry_after_ms == 0 =>
            {
                Ok(Self::UninitializedState)
            }
            (-1, "malformed_daily_cost_state" | "malformed_monthly_cost_state")
                if retry_after_ms == 0 =>
            {
                Ok(Self::MalformedState)
            }
            (-1, "invalid_arguments" | "invalid_server_time") if retry_after_ms == 0 => {
                Ok(Self::ScriptFailure)
            }
            _ => Err(LimitError::UnexpectedResponse),
        }
    }
}

type RawReservationScriptResponse = (i64, i64, String, i64, i64, i64);

impl ReservationScriptResult {
    pub(super) fn parse_value(value: &redis::Value) -> Result<Self, LimitError> {
        let response = RawReservationScriptResponse::from_redis_value_ref(value)
            .map_err(|_| LimitError::UnexpectedResponse)?;
        Self::parse(response)
    }

    pub(super) fn parse(
        (version, status, detail, retry_after_ms, window_id, lease_expiry_ms): RawReservationScriptResponse,
    ) -> Result<Self, LimitError> {
        if version != SCRIPT_RESPONSE_VERSION
            || !(0..=MAX_LUA_INTEGER).contains(&window_id)
            || !(0..=MAX_LUA_INTEGER).contains(&lease_expiry_ms)
        {
            return Err(LimitError::UnexpectedResponse);
        }

        match (status, detail.as_str()) {
            (1, "ok") if retry_after_ms == 0 && window_id > 0 => Ok(Self::Granted {
                window_id,
                concurrency_expires_at_ms: (lease_expiry_ms > 0).then_some(lease_expiry_ms),
            }),
            (0, "rpm" | "tpm")
                if (1..=FIXED_WINDOW_MS).contains(&retry_after_ms)
                    && window_id > 0
                    && lease_expiry_ms == 0 =>
            {
                Ok(Self::Rejected {
                    dimension: if detail == "rpm" {
                        LimitDimension::Requests
                    } else {
                        LimitDimension::Tokens
                    },
                    retry_after_ms: retry_after_ms as u64,
                })
            }
            (0, "concurrency")
                if (1..=MAX_LUA_INTEGER).contains(&retry_after_ms)
                    && window_id > 0
                    && lease_expiry_ms == 0 =>
            {
                Ok(Self::Rejected {
                    dimension: LimitDimension::Concurrency,
                    retry_after_ms: retry_after_ms as u64,
                })
            }
            (-1, "malformed_rate_state" | "malformed_concurrency_state")
                if retry_after_ms == 0 && window_id > 0 && lease_expiry_ms == 0 =>
            {
                Ok(Self::MalformedState)
            }
            (-1, "invalid_arguments" | "invalid_server_time")
                if retry_after_ms == 0 && window_id == 0 && lease_expiry_ms == 0 =>
            {
                Ok(Self::ScriptFailure)
            }
            _ => Err(LimitError::UnexpectedResponse),
        }
    }
}
