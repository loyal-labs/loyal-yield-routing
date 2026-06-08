use base64::{
    engine::general_purpose::{
        STANDARD as BASE64_STANDARD, STANDARD_NO_PAD as BASE64_STANDARD_NO_PAD,
    },
    Engine as _,
};
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
    #[error("yield router keypair must be JSON, hex, base58, or base64 encoded")]
    InvalidEncoding,
    #[error("yield router keypair must decode to 32 or 64 bytes, got {lengths}")]
    InvalidLength { lengths: String },
    #[error("yield router keypair bytes do not describe a valid Solana keypair")]
    InvalidKeypair,
}

pub fn yield_router_keypair_from_env() -> Result<Keypair, PolicySignerError> {
    let value = env::var(YIELD_ROUTER_KEYPAIR_ENV).map_err(|_| PolicySignerError::MissingEnv {
        name: YIELD_ROUTER_KEYPAIR_ENV,
    })?;
    keypair_from_secret_string(&value)
}

pub fn keypair_from_secret_string(value: &str) -> Result<Keypair, PolicySignerError> {
    let value = value.trim();
    if value.starts_with('[') {
        let bytes = serde_json::from_str::<Vec<u8>>(value)
            .map_err(|_| PolicySignerError::InvalidEncoding)?;
        return keypair_from_bytes(bytes);
    }

    let mut decoded_lengths = Vec::new();
    let mut decoded_invalid_keypair = false;
    for bytes in decode_secret_candidates(value) {
        match keypair_from_bytes(bytes) {
            Ok(keypair) => return Ok(keypair),
            Err(PolicySignerError::InvalidLength { lengths }) => decoded_lengths.push(lengths),
            Err(PolicySignerError::InvalidKeypair) => decoded_invalid_keypair = true,
            Err(error) => return Err(error),
        }
    }

    if decoded_lengths.is_empty() {
        return Err(PolicySignerError::InvalidEncoding);
    }

    if decoded_invalid_keypair {
        return Err(PolicySignerError::InvalidKeypair);
    }

    decoded_lengths.sort();
    decoded_lengths.dedup();
    Err(PolicySignerError::InvalidLength {
        lengths: decoded_lengths.join(", "),
    })
}

pub fn keypair_from_hex(value: &str) -> Result<Keypair, PolicySignerError> {
    let bytes = decode_hex(value).map_err(|_| PolicySignerError::InvalidEncoding)?;
    keypair_from_bytes(bytes)
}

fn keypair_from_bytes(bytes: Vec<u8>) -> Result<Keypair, PolicySignerError> {
    match bytes.len() {
        SOLANA_SECRET_KEY_LENGTH => {
            let mut seed = [0u8; SOLANA_SECRET_KEY_LENGTH];
            seed.copy_from_slice(&bytes);
            Ok(Keypair::new_from_array(seed))
        }
        SOLANA_KEYPAIR_LENGTH => {
            Keypair::try_from(bytes.as_slice()).map_err(|_| PolicySignerError::InvalidKeypair)
        }
        length => Err(PolicySignerError::InvalidLength {
            lengths: length.to_string(),
        }),
    }
}

fn decode_secret_candidates(value: &str) -> Vec<Vec<u8>> {
    let mut candidates = Vec::new();
    if let Ok(bytes) = decode_hex(value) {
        candidates.push(bytes);
    }
    if let Ok(bytes) = bs58::decode(value).into_vec() {
        candidates.push(bytes);
    }
    if let Ok(bytes) = BASE64_STANDARD.decode(value) {
        candidates.push(bytes);
    }
    if let Ok(bytes) = BASE64_STANDARD_NO_PAD.decode(value) {
        candidates.push(bytes);
    }
    candidates
}

fn decode_hex(value: &str) -> Result<Vec<u8>, ()> {
    let value = value.trim();
    let value = value
        .strip_prefix("0x")
        .or_else(|| value.strip_prefix("0X"))
        .unwrap_or(value);

    if value.len() % 2 != 0 || !value.as_bytes().iter().all(u8::is_ascii_hexdigit) {
        return Err(());
    }

    value
        .as_bytes()
        .chunks_exact(2)
        .map(|chunk| {
            let high = hex_nibble(chunk[0])?;
            let low = hex_nibble(chunk[1])?;
            Ok((high << 4) | low)
        })
        .collect()
}

fn hex_nibble(byte: u8) -> Result<u8, ()> {
    match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        b'A'..=b'F' => Ok(byte - b'A' + 10),
        _ => Err(()),
    }
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
    fn parses_solana_keypair_json_array() {
        let keypair = Keypair::new();
        let encoded = serde_json::to_string(&keypair.to_bytes().to_vec()).unwrap();

        let parsed = keypair_from_secret_string(&encoded).unwrap();

        assert_eq!(parsed.pubkey(), keypair.pubkey());
    }

    #[test]
    fn rejects_wrong_keypair_length_without_echoing_secret() {
        let error = keypair_from_hex("010203").unwrap_err();

        assert!(matches!(
            error,
            PolicySignerError::InvalidLength { ref lengths } if lengths == "3"
        ));
        assert!(!error.to_string().contains("010203"));
    }

    fn hex_encode(bytes: &[u8]) -> String {
        const HEX: &[u8; 16] = b"0123456789abcdef";
        let mut output = String::with_capacity(bytes.len() * 2);
        for byte in bytes {
            output.push(HEX[(byte >> 4) as usize] as char);
            output.push(HEX[(byte & 0x0f) as usize] as char);
        }
        output
    }
}
