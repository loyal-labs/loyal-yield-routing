//! Pseudonymous actor identifiers for correlating operational events.

use std::{env, fmt};

use hmac::{Hmac, Mac};
use sha2::Sha256;

/// Server-only HMAC secret shared with the frontend observability pipeline.
pub const ACTOR_HMAC_SECRET_ENV: &str = "OBSERVABILITY_ACTOR_HMAC_SECRET";

const ACTOR_ID_PREFIX: &str = "actor:v1:";
const MIN_SECRET_UTF16_CODE_UNITS: usize = 32;

/// A pseudonymous actor identifier safe to attach to observability events.
///
/// This value supports correlation but is not anonymous. Treat it as telemetry
/// data and do not expose it to end users.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct ObservabilityActorId(String);

impl ObservabilityActorId {
    /// Returns the `actor:v1:<64 lowercase hex characters>` representation.
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl fmt::Display for ObservabilityActorId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

impl AsRef<str> for ObservabilityActorId {
    fn as_ref(&self) -> &str {
        self.as_str()
    }
}

/// Derives the same actor ID as the Loyal frontend observability pipeline.
///
/// The HMAC input is exactly `v1|<deployment_environment>|<wallet_address>`.
/// Only surrounding whitespace is removed; the wallet address is otherwise
/// preserved because Solana base58 addresses are case-sensitive.
pub fn derive_observability_actor_id(
    deployment_environment: &str,
    secret: &str,
    wallet_address: &str,
) -> Option<ObservabilityActorId> {
    let secret = secret.trim();
    let environment = deployment_environment.trim();
    let wallet_address = wallet_address.trim();

    // JavaScript's String.length counts UTF-16 code units. Matching that check
    // keeps this contract aligned with the frontend for non-ASCII secrets too.
    if secret.encode_utf16().count() < MIN_SECRET_UTF16_CODE_UNITS
        || environment.is_empty()
        || wallet_address.is_empty()
    {
        return None;
    }

    let mut mac = Hmac::<Sha256>::new_from_slice(secret.as_bytes()).ok()?;
    mac.update(b"v1|");
    mac.update(environment.as_bytes());
    mac.update(b"|");
    mac.update(wallet_address.as_bytes());

    Some(ObservabilityActorId(format!(
        "{ACTOR_ID_PREFIX}{}",
        hex::encode(mac.finalize().into_bytes())
    )))
}

/// Reads [`ACTOR_HMAC_SECRET_ENV`] and derives an actor ID without retaining
/// the secret in observability configuration or debug output.
pub fn derive_observability_actor_id_from_env(
    deployment_environment: &str,
    wallet_address: &str,
) -> Option<ObservabilityActorId> {
    let secret = env::var(ACTOR_HMAC_SECRET_ENV).ok()?;
    derive_observability_actor_id(deployment_environment, &secret, wallet_address)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn matches_frontend_hmac_sha256_contract() {
        let actor_id = derive_observability_actor_id(
            "production",
            "0123456789abcdef0123456789abcdef",
            "11111111111111111111111111111111",
        )
        .expect("valid inputs should produce an actor ID");

        assert_eq!(
            actor_id.as_str(),
            "actor:v1:1f03f7224c37bb7b003db1f3d2a4d46d17194c368f393ccea4c7e75aadf77151"
        );
    }

    #[test]
    fn rejects_missing_identity_material() {
        assert!(derive_observability_actor_id("", "a".repeat(32).as_str(), "wallet").is_none());
        assert!(derive_observability_actor_id("production", "short", "wallet").is_none());
        assert!(derive_observability_actor_id("production", "a".repeat(32).as_str(), "").is_none());
    }
}
