use solana_sdk::signature::Keypair;
use std::env;
use thiserror::Error;

pub const YIELD_ROUTER_KEYPAIR_ENV: &str = "YIELD_ROUTER_KEYPAIR";
const SOLANA_SECRET_KEY_LENGTH: usize = 32;
const SOLANA_KEYPAIR_LENGTH: usize = 64;

#[derive(Debug, Error)]
pub enum PolicySignerError {
    #[error("{name} is not set")]
    MissingEnv { name: &'static str },
    #[error("yield router keypair must be hex encoded")]
    InvalidHex,
    #[error("yield router keypair must decode to 32 or 64 bytes, got {length}")]
    InvalidLength { length: usize },
    #[error("yield router keypair bytes do not describe a valid Solana keypair")]
    InvalidKeypair,
}

pub fn yield_router_keypair_from_env() -> Result<Keypair, PolicySignerError> {
    let value = env::var(YIELD_ROUTER_KEYPAIR_ENV).map_err(|_| PolicySignerError::MissingEnv {
        name: YIELD_ROUTER_KEYPAIR_ENV,
    })?;
    keypair_from_hex(&value)
}

pub fn keypair_from_hex(value: &str) -> Result<Keypair, PolicySignerError> {
    let bytes = decode_hex(value)?;
    match bytes.len() {
        SOLANA_SECRET_KEY_LENGTH => {
            let mut seed = [0u8; SOLANA_SECRET_KEY_LENGTH];
            seed.copy_from_slice(&bytes);
            Ok(Keypair::new_from_array(seed))
        }
        SOLANA_KEYPAIR_LENGTH => {
            Keypair::try_from(bytes.as_slice()).map_err(|_| PolicySignerError::InvalidKeypair)
        }
        length => Err(PolicySignerError::InvalidLength { length }),
    }
}

fn decode_hex(value: &str) -> Result<Vec<u8>, PolicySignerError> {
    let value = value.trim();
    let value = value
        .strip_prefix("0x")
        .or_else(|| value.strip_prefix("0X"))
        .unwrap_or(value);

    hex::decode(value).map_err(|_| PolicySignerError::InvalidHex)
}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_sdk::signer::Signer;

    #[test]
    fn parses_solana_keypair_hex() {
        let keypair = Keypair::new();
        let encoded = hex_encode(&keypair.to_bytes());

        let parsed = keypair_from_hex(&encoded).unwrap();

        assert_eq!(parsed.pubkey(), keypair.pubkey());
    }

    #[test]
    fn parses_private_seed_hex() {
        let seed = [7u8; SOLANA_SECRET_KEY_LENGTH];
        let expected = Keypair::new_from_array(seed);
        let encoded = hex_encode(&seed);

        let parsed = keypair_from_hex(&encoded).unwrap();

        assert_eq!(parsed.pubkey(), expected.pubkey());
    }

    #[test]
    fn accepts_hex_prefix() {
        let seed = [9u8; SOLANA_SECRET_KEY_LENGTH];
        let expected = Keypair::new_from_array(seed);
        let encoded = format!("0x{}", hex_encode(&seed));

        let parsed = keypair_from_hex(&encoded).unwrap();

        assert_eq!(parsed.pubkey(), expected.pubkey());
    }

    #[test]
    fn rejects_wrong_keypair_length_without_echoing_secret() {
        let error = keypair_from_hex("010203").unwrap_err();

        assert!(matches!(
            error,
            PolicySignerError::InvalidLength { length: 3 }
        ));
        assert!(!error.to_string().contains("010203"));
    }

    fn hex_encode(bytes: &[u8]) -> String {
        hex::encode(bytes)
    }
}
