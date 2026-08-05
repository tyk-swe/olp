//! Process composition: CLI, dependency construction, runtime assembly,
//! listener lifecycle, and mode-valid state finalization.

pub(crate) mod cli;
pub(crate) mod connectors;
pub(crate) mod media_spool;
pub(crate) mod mode_dependencies;
pub(crate) mod provider_adapter;
pub(crate) mod state;
