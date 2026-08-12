use crate::domain::{Usage, UsageObservation};
use serde::{Serialize, Serializer, ser::Error as _};

pub(in crate::protocols) fn serialize_required_option<T, S>(
    value: &Option<T>,
    serializer: S,
) -> Result<S::Ok, S::Error>
where
    T: Serialize,
    S: Serializer,
{
    value
        .as_ref()
        .ok_or_else(|| S::Error::custom("required usage counter is absent"))?
        .serialize(serializer)
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(super) struct ObservedUsage {
    pub input_tokens: Option<u64>,
    pub output_tokens: Option<u64>,
    pub total_tokens: Option<u64>,
    pub cached_input_tokens: Option<u64>,
    pub reasoning_tokens: Option<u64>,
}

impl ObservedUsage {
    #[must_use]
    pub const fn observation(self) -> UsageObservation {
        UsageObservation {
            input_tokens: self.input_tokens,
            output_tokens: self.output_tokens,
            total_tokens: self.total_tokens,
            cached_input_tokens: self.cached_input_tokens,
            reasoning_tokens: self.reasoning_tokens,
        }
    }

    /// Produces canonical usage only when all required counters were present
    /// and the provider's redundant total agrees with its components.
    pub fn with_exact_total(self) -> Result<Option<Usage>, InvalidUsage> {
        for counter in [
            self.input_tokens,
            self.output_tokens,
            self.total_tokens,
            self.cached_input_tokens,
            self.reasoning_tokens,
        ] {
            validate_counter(counter)?;
        }
        let (Some(input_tokens), Some(output_tokens)) = (self.input_tokens, self.output_tokens)
        else {
            return Ok(None);
        };
        if self
            .cached_input_tokens
            .is_some_and(|cached| cached > input_tokens)
            || self
                .reasoning_tokens
                .is_some_and(|reasoning| reasoning > output_tokens)
        {
            return Err(InvalidUsage);
        }
        let expected_total = input_tokens
            .checked_add(output_tokens)
            .ok_or(InvalidUsage)?;
        let Some(total_tokens) = self.total_tokens else {
            return Ok(None);
        };
        if total_tokens != expected_total {
            return Err(InvalidUsage);
        }
        Ok(Some(Usage {
            input_tokens,
            output_tokens,
            total_tokens,
            cached_input_tokens: self.cached_input_tokens,
            reasoning_tokens: self.reasoning_tokens,
        }))
    }
}

pub(super) fn validate_counter(value: Option<u64>) -> Result<(), InvalidUsage> {
    if value.is_some_and(|value| value > i64::MAX as u64) {
        Err(InvalidUsage)
    } else {
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct InvalidUsage;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocols::{anthropic, gemini, openai};
    use serde::de::DeserializeOwned;
    use serde_json::{Value, json};

    #[test]
    fn required_counter_presence_controls_canonical_usage() {
        for observation in [
            ObservedUsage::default(),
            ObservedUsage {
                input_tokens: Some(1),
                ..ObservedUsage::default()
            },
            ObservedUsage {
                output_tokens: Some(2),
                total_tokens: Some(2),
                ..ObservedUsage::default()
            },
            ObservedUsage {
                input_tokens: Some(1),
                output_tokens: Some(2),
                ..ObservedUsage::default()
            },
        ] {
            assert_eq!(observation.with_exact_total(), Ok(None));
        }

        let zero = ObservedUsage {
            input_tokens: Some(0),
            output_tokens: Some(0),
            total_tokens: Some(0),
            ..ObservedUsage::default()
        }
        .with_exact_total()
        .unwrap()
        .unwrap();
        assert_eq!(zero.input_tokens, 0);
        assert_eq!(zero.output_tokens, 0);
        assert_eq!(zero.total_tokens, 0);
    }

    #[test]
    fn contradictory_and_overflowing_totals_are_rejected() {
        for observation in [
            ObservedUsage {
                input_tokens: Some(1),
                output_tokens: Some(2),
                total_tokens: Some(4),
                ..ObservedUsage::default()
            },
            ObservedUsage {
                input_tokens: Some(u64::MAX),
                output_tokens: Some(1),
                total_tokens: Some(u64::MAX),
                ..ObservedUsage::default()
            },
            ObservedUsage {
                cached_input_tokens: Some(u64::MAX),
                ..ObservedUsage::default()
            },
            ObservedUsage {
                input_tokens: Some(2),
                output_tokens: Some(1),
                total_tokens: Some(3),
                cached_input_tokens: Some(3),
                ..ObservedUsage::default()
            },
            ObservedUsage {
                input_tokens: Some(1),
                output_tokens: Some(2),
                total_tokens: Some(3),
                reasoning_tokens: Some(3),
                ..ObservedUsage::default()
            },
        ] {
            assert_eq!(observation.with_exact_total(), Err(InvalidUsage));
        }
    }

    #[test]
    fn provider_usage_dtos_preserve_presence_and_reject_malformed_counters() {
        assert_missing_and_malformed::<openai::ChatUsage>(
            &["prompt_tokens", "completion_tokens", "total_tokens"],
            |usage| {
                [
                    usage.prompt_tokens,
                    usage.completion_tokens,
                    usage.total_tokens,
                ]
            },
        );
        assert_missing_and_malformed::<openai::ResponseUsage>(
            &["input_tokens", "output_tokens", "total_tokens"],
            |usage| [usage.input_tokens, usage.output_tokens, usage.total_tokens],
        );
        assert_missing_and_malformed::<openai::OpenAiImageUsage>(
            &["input_tokens", "output_tokens", "total_tokens"],
            |usage| [usage.input_tokens, usage.output_tokens, usage.total_tokens],
        );
        assert_missing_and_malformed::<gemini::UsageMetadata>(
            &[
                "promptTokenCount",
                "candidatesTokenCount",
                "totalTokenCount",
            ],
            |usage| {
                [
                    usage.prompt_token_count,
                    usage.candidates_token_count,
                    usage.total_token_count,
                ]
            },
        );

        for field in ["input_tokens", "output_tokens"] {
            assert_optional_counter::<anthropic::Usage>(field);
        }
        let usage: anthropic::Usage = serde_json::from_value(json!({})).unwrap();
        assert_eq!(usage.input_tokens, None);
        assert_eq!(usage.output_tokens, None);
        let usage: anthropic::Usage = serde_json::from_value(json!({
            "input_tokens": 0,
            "output_tokens": 0
        }))
        .unwrap();
        assert_eq!(usage.input_tokens, Some(0));
        assert_eq!(usage.output_tokens, Some(0));
    }

    #[test]
    fn omitted_optional_details_remain_absent() {
        let chat: openai::ChatUsage = serde_json::from_value(json!({
            "prompt_tokens": 1,
            "completion_tokens": 2,
            "total_tokens": 3,
            "prompt_tokens_details": {},
            "completion_tokens_details": {}
        }))
        .unwrap();
        assert_eq!(chat.prompt_tokens_details.unwrap().cached_tokens, None);
        assert_eq!(
            chat.completion_tokens_details.unwrap().reasoning_tokens,
            None
        );

        let responses: openai::ResponseUsage = serde_json::from_value(json!({
            "input_tokens": 1,
            "output_tokens": 2,
            "total_tokens": 3,
            "input_tokens_details": {},
            "output_tokens_details": {}
        }))
        .unwrap();
        assert_eq!(responses.input_tokens_details.unwrap().cached_tokens, None);
        assert_eq!(
            responses.output_tokens_details.unwrap().reasoning_tokens,
            None
        );
    }

    #[test]
    fn incomplete_decode_dtos_cannot_be_emitted_as_valid_provider_usage() {
        assert!(serde_json::to_value(openai::ChatUsage::default()).is_err());
        assert!(serde_json::to_value(anthropic::Usage::default()).is_err());
        assert!(serde_json::to_value(gemini::UsageMetadata::default()).is_err());

        let zero: openai::ChatUsage = serde_json::from_value(json!({
            "prompt_tokens": 0,
            "completion_tokens": 0,
            "total_tokens": 0
        }))
        .unwrap();
        assert_eq!(serde_json::to_value(zero).unwrap()["total_tokens"], 0);
    }

    fn assert_missing_and_malformed<T>(fields: &[&str; 3], counters: impl Fn(T) -> [Option<u64>; 3])
    where
        T: DeserializeOwned,
    {
        let empty: T = serde_json::from_value(json!({})).unwrap();
        assert_eq!(counters(empty), [None, None, None]);
        let nulls: T = serde_json::from_value(json!({
            fields[0]: null,
            fields[1]: null,
            fields[2]: null,
        }))
        .unwrap();
        assert_eq!(counters(nulls), [None, None, None]);
        let zeros: T = serde_json::from_value(json!({
            fields[0]: 0,
            fields[1]: 0,
            fields[2]: 0,
        }))
        .unwrap();
        assert_eq!(counters(zeros), [Some(0), Some(0), Some(0)]);
        for field in fields {
            assert_optional_counter::<T>(field);
        }
    }

    fn assert_optional_counter<T: DeserializeOwned>(field: &str) {
        for malformed in [json!(-1), json!(1.5), json!(true), json!("1")] {
            let mut object = serde_json::Map::new();
            object.insert(field.to_owned(), malformed);
            assert!(serde_json::from_value::<T>(Value::Object(object)).is_err());
        }
        let overflowing = format!(r#"{{"{field}":18446744073709551616}}"#);
        assert!(serde_json::from_str::<T>(&overflowing).is_err());
    }
}
