use solana_sdk::signature::Keypair;
use std::env;
use thiserror::Error;

pub const SOLANA_TESTING_PK_ENV: &str = "SOLANA_TESTING_PK";
pub const POLICY_KEYPAIR_ENV: &str = "POLICY_KEYPAIR";
pub const YIELD_ROUTER_KEYPAIR_ENV: &str = "YIELD_ROUTER_KEYPAIR";
pub const YIELD_ALT_MANAGER_KEYPAIR_ENV: &str = "YIELD_ALT_MANAGER_KEYPAIR";
const SOLANA_SECRET_KEY_LENGTH: usize = 32;
const SOLANA_KEYPAIR_LENGTH: usize = 64;

#[derive(Debug, Error)]
pub enum PolicySignerError {
    #[error("{name} is not set")]
    MissingEnv { name: &'static str },
    #[error("Solana keypair must be hex encoded")]
    InvalidHex,
    #[error("Solana keypair must be base58 encoded")]
    InvalidBase58,
    #[error("Solana keypair JSON must be a byte array")]
    InvalidJson,
    #[error("Solana keypair must decode to 32 or 64 bytes, got {length}")]
    InvalidLength { length: usize },
    #[error("Solana keypair bytes do not describe a valid Solana keypair")]
    InvalidKeypair,
}

pub fn solana_testing_keypair_from_env() -> Result<Keypair, PolicySignerError> {
    keypair_from_env(SOLANA_TESTING_PK_ENV)
}

pub fn policy_keypair_from_env() -> Result<Keypair, PolicySignerError> {
    keypair_from_env(POLICY_KEYPAIR_ENV)
}

pub fn yield_router_keypair_from_env() -> Result<Keypair, PolicySignerError> {
    keypair_from_env(YIELD_ROUTER_KEYPAIR_ENV)
}

pub fn yield_alt_manager_keypair_from_env() -> Result<Keypair, PolicySignerError> {
    keypair_from_env(YIELD_ALT_MANAGER_KEYPAIR_ENV)
}

pub fn keypair_from_env(name: &'static str) -> Result<Keypair, PolicySignerError> {
    let value = env::var(name).map_err(|_| PolicySignerError::MissingEnv { name })?;
    keypair_from_string(&value)
}

pub fn keypair_from_string(value: &str) -> Result<Keypair, PolicySignerError> {
    let value = value.trim();
    if value.starts_with('[') {
        let bytes: Vec<u8> =
            serde_json::from_str(value).map_err(|_| PolicySignerError::InvalidJson)?;
        return keypair_from_bytes(&bytes);
    }

    if is_hex_string(value) {
        return keypair_from_hex(value);
    }

    let bytes = bs58::decode(value)
        .into_vec()
        .map_err(|_| PolicySignerError::InvalidBase58)?;
    keypair_from_bytes(&bytes)
}

pub fn keypair_from_hex(value: &str) -> Result<Keypair, PolicySignerError> {
    let bytes = decode_hex(value)?;
    keypair_from_bytes(&bytes)
}

fn keypair_from_bytes(bytes: &[u8]) -> Result<Keypair, PolicySignerError> {
    match bytes.len() {
        SOLANA_SECRET_KEY_LENGTH => {
            let mut seed = [0u8; SOLANA_SECRET_KEY_LENGTH];
            seed.copy_from_slice(&bytes);
            Ok(Keypair::new_from_array(seed))
        }
        SOLANA_KEYPAIR_LENGTH => {
            Keypair::try_from(bytes).map_err(|_| PolicySignerError::InvalidKeypair)
        }
        length => Err(PolicySignerError::InvalidLength { length }),
    }
}

fn is_hex_string(value: &str) -> bool {
    let value = value
        .strip_prefix("0x")
        .or_else(|| value.strip_prefix("0X"))
        .unwrap_or(value);
    !value.is_empty()
        && value.len() % 2 == 0
        && value.as_bytes().iter().all(|byte| byte.is_ascii_hexdigit())
}

fn decode_hex(value: &str) -> Result<Vec<u8>, PolicySignerError> {
    let value = value.trim();
    let value = value
        .strip_prefix("0x")
        .or_else(|| value.strip_prefix("0X"))
        .unwrap_or(value);

    if value.len() % 2 != 0 {
        return Err(PolicySignerError::InvalidHex);
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

fn hex_nibble(byte: u8) -> Result<u8, PolicySignerError> {
    match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        b'A'..=b'F' => Ok(byte - b'A' + 10),
        _ => Err(PolicySignerError::InvalidHex),
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

        let parsed = keypair_from_string(&encoded).unwrap();

        assert_eq!(parsed.pubkey(), keypair.pubkey());
    }

    #[test]
    fn parses_solana_keypair_base58() {
        let keypair = Keypair::new();
        let encoded = bs58::encode(keypair.to_bytes()).into_string();

        let parsed = keypair_from_string(&encoded).unwrap();

        assert_eq!(parsed.pubkey(), keypair.pubkey());
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
        const HEX: &[u8; 16] = b"0123456789abcdef";
        let mut output = String::with_capacity(bytes.len() * 2);
        for byte in bytes {
            output.push(HEX[(byte >> 4) as usize] as char);
            output.push(HEX[(byte & 0x0f) as usize] as char);
        }
        output
    }
}
