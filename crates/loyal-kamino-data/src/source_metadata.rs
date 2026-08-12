pub const CONFIRMED_COMMITMENT: &str = "confirmed";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct UpdateSourceMetadata {
    pub source: &'static str,
    pub source_commitment: &'static str,
}
