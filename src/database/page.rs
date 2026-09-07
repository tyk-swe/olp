use uuid::Uuid;

#[derive(Clone, Debug)]
pub struct ConfigurationPage<T> {
    pub items: Vec<T>,
    pub next_cursor: Option<Uuid>,
}
