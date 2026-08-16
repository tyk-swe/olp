pub mod audit;
pub mod cursor;
pub mod health;
pub mod pricing;
pub mod requests;
pub mod runtime;
pub mod settings;

pub(crate) const MAX_PAGE_SIZE: u16 = 200;

#[cfg(test)]
mod tests;
