//! Process composition: CLI, dependency construction, runtime assembly,
//! listener lifecycle, and mode-valid state finalization.

pub mod cli;
pub(crate) mod connectors;
#[cfg(feature = "test-util")]
pub mod media_spool;
#[cfg(not(feature = "test-util"))]
pub(crate) mod media_spool;
#[cfg(feature = "test-util")]
pub mod mode_dependencies;
#[cfg(not(feature = "test-util"))]
pub(crate) mod mode_dependencies;
pub(crate) mod provider_adapter;
#[cfg(feature = "test-util")]
pub mod state;
#[cfg(not(feature = "test-util"))]
pub(crate) mod state;
