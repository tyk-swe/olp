use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct MessagesRequest {
    pub model: String,
    pub messages: Vec<Message>,
    pub max_tokens: u32,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub system: Option<SystemPrompt>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub stop_sequences: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub temperature: Option<f32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub top_p: Option<f32>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<Tool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool_choice: Option<ToolChoice>,
    #[serde(default, skip_serializing_if = "is_false")]
    pub stream: bool,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

const fn is_false(value: &bool) -> bool {
    !*value
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct CountTokensRequest {
    pub model: String,
    pub messages: Vec<Message>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub system: Option<SystemPrompt>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<Tool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool_choice: Option<ToolChoice>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct CountTokensResponse {
    pub input_tokens: u64,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct Message {
    pub role: Role,
    pub content: MessageContent,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Role {
    User,
    Assistant,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum MessageContent {
    Text(String),
    Blocks(Vec<ContentBlock>),
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum SystemPrompt {
    Text(String),
    Blocks(Vec<TextBlock>),
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum ContentBlock {
    Text(TextBlock),
    Image(ImageBlock),
    ToolUse(ToolUseBlock),
    ToolResult(ToolResultBlock),
    Thinking(ThinkingBlock),
    RedactedThinking(RedactedThinkingBlock),
    Unknown(Value),
}

impl ContentBlock {
    #[must_use]
    pub fn kind(&self) -> Option<&str> {
        match self {
            Self::Text(block) => Some(&block.kind),
            Self::Image(block) => Some(&block.kind),
            Self::ToolUse(block) => Some(&block.kind),
            Self::ToolResult(block) => Some(&block.kind),
            Self::Thinking(block) => Some(&block.kind),
            Self::RedactedThinking(block) => Some(&block.kind),
            Self::Unknown(value) => value.get("type").and_then(Value::as_str),
        }
    }

    #[must_use]
    pub fn as_value(&self) -> Value {
        serde_json::to_value(self).unwrap_or(Value::Null)
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct TextBlock {
    #[serde(rename = "type")]
    pub kind: String,
    pub text: String,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ImageBlock {
    #[serde(rename = "type")]
    pub kind: String,
    pub source: MediaSource,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct MediaSource {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub media_type: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub data: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ToolUseBlock {
    #[serde(rename = "type")]
    pub kind: String,
    pub id: String,
    pub name: String,
    #[serde(default = "empty_object")]
    pub input: Value,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

fn empty_object() -> Value {
    Value::Object(Default::default())
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ToolResultBlock {
    #[serde(rename = "type")]
    pub kind: String,
    pub tool_use_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content: Option<ToolResultContent>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub is_error: Option<bool>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum ToolResultContent {
    Text(String),
    Blocks(Vec<ContentBlock>),
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ThinkingBlock {
    #[serde(rename = "type")]
    pub kind: String,
    pub thinking: String,
    pub signature: String,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct RedactedThinkingBlock {
    #[serde(rename = "type")]
    pub kind: String,
    pub data: String,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct Tool {
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input_schema: Option<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    #[serde(rename = "type")]
    pub kind: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ToolChoice {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub disable_parallel_tool_use: Option<bool>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct MessagesResponse {
    pub id: String,
    #[serde(rename = "type")]
    pub kind: String,
    pub role: Role,
    pub content: Vec<ContentBlock>,
    pub model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub stop_reason: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub stop_sequence: Option<String>,
    pub usage: Usage,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct Usage {
    #[serde(default)]
    pub input_tokens: u64,
    #[serde(default)]
    pub output_tokens: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cache_creation_input_tokens: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cache_read_input_tokens: Option<u64>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}
