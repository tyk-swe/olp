//! Compilation, publication, and durable activation records for runtime state.

use std::fmt;

use chrono::{DateTime, Utc};
use uuid::Uuid;

mod compiler;
mod outbox;
mod releases;

pub use compiler::RuntimeCompileError;
pub(crate) use compiler::{
    compile_and_publish_runtime_in_transaction, lock_runtime_publication, prepare_runtime_mutation,
};

#[derive(Clone)]
pub struct PublishedRuntimeRelease {
    pub generation_id: Uuid,
    pub sequence: i64,
    pub payload: Vec<u8>,
    pub payload_sha256: [u8; 32],
    pub created_at: DateTime<Utc>,
}

impl fmt::Debug for PublishedRuntimeRelease {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PublishedRuntimeRelease")
            .field("generation_id", &self.generation_id)
            .field("sequence", &self.sequence)
            .field("payload", &"[REDACTED]")
            .field("payload_sha256", &self.payload_sha256)
            .field("created_at", &self.created_at)
            .finish()
    }
}

#[derive(Clone)]
pub struct OutboxRecord {
    pub id: Uuid,
    pub topic: String,
    pub aggregate_id: Uuid,
    pub payload: Vec<u8>,
    pub created_at: DateTime<Utc>,
}

impl fmt::Debug for OutboxRecord {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OutboxRecord")
            .field("id", &self.id)
            .field("topic", &self.topic)
            .field("aggregate_id", &self.aggregate_id)
            .field("payload", &"[REDACTED]")
            .field("created_at", &self.created_at)
            .finish()
    }
}

#[cfg(test)]
mod tests;
