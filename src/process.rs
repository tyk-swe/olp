//! Process composition: CLI, dependency construction, runtime assembly,
//! listener lifecycle, and mode-valid state finalization.

pub mod cli;

pub mod error;

pub mod mode;

pub mod mode_dependencies;

pub mod state;

pub mod workers;
