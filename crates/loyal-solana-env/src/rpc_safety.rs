use std::str::FromStr;

use solana_sdk::hash::Hash;

const MAINNET_BETA_GENESIS_HASH: &str = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const DEVNET_GENESIS_HASH: &str = "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG";
const TESTNET_GENESIS_HASH: &str = "4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY";

pub fn validate_rpc_genesis_hash(cluster: &str, observed: Hash) -> Result<(), String> {
    let mainnet = parse_genesis_hash(MAINNET_BETA_GENESIS_HASH);
    let devnet = parse_genesis_hash(DEVNET_GENESIS_HASH);
    let testnet = parse_genesis_hash(TESTNET_GENESIS_HASH);
    let matches = match cluster {
        "mainnet-beta" => observed == mainnet,
        "devnet" => observed == devnet,
        "testnet" => observed == testnet,
        "localnet" => observed != mainnet && observed != devnet && observed != testnet,
        _ => {
            return Err(format!(
                "unsupported explicit cluster {cluster:?}; expected mainnet-beta, devnet, testnet, or localnet"
            ))
        }
    };
    if !matches {
        return Err(format!(
            "RPC genesis hash {observed} does not match explicit cluster {cluster}"
        ));
    }
    Ok(())
}

/// Produces a bounded fatal-error message without endpoint credentials.
pub fn redacted_external_error(message: &str) -> String {
    let mut suppress_following_tokens = 0_usize;
    message
        .split_whitespace()
        .map(|token| {
            if suppress_following_tokens != 0 {
                suppress_following_tokens -= 1;
                return "[redacted-external-endpoint]";
            }
            let lowercase = token.to_ascii_lowercase();
            let inline_secret = [
                "api-key=",
                "api_key=",
                "apikey=",
                "access-token=",
                "access_token=",
                "token=",
                "password=",
                "authorization=",
            ]
            .iter()
            .any(|marker| lowercase.contains(marker));
            let colon_secret_markers = [
                "api-key:",
                "api_key:",
                "apikey:",
                "access-token:",
                "access_token:",
                "token:",
                "password:",
                "authorization:",
            ];
            let separated_secret = colon_secret_markers
                .iter()
                .any(|marker| lowercase.ends_with(marker));
            let colon_secret = colon_secret_markers
                .iter()
                .any(|marker| lowercase.contains(marker));
            let authorization_header = lowercase.ends_with("authorization:");
            let authorization_scheme = matches!(
                lowercase.trim_matches(|character: char| !character.is_ascii_alphabetic()),
                "bearer" | "basic"
            );
            if authorization_header {
                suppress_following_tokens = 2;
                "[redacted-external-endpoint]"
            } else if separated_secret || authorization_scheme {
                suppress_following_tokens = 1;
                "[redacted-external-endpoint]"
            } else if token.contains("://") || inline_secret || colon_secret {
                "[redacted-external-endpoint]"
            } else {
                token
            }
        })
        .collect::<Vec<_>>()
        .join(" ")
        .chars()
        .take(512)
        .collect()
}

fn parse_genesis_hash(value: &str) -> Hash {
    Hash::from_str(value).expect("canonical Solana genesis hash constant must parse")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reusable_alt_rpc_genesis_matches_only_the_explicit_cluster() {
        let mainnet = parse_genesis_hash(MAINNET_BETA_GENESIS_HASH);
        let devnet = parse_genesis_hash(DEVNET_GENESIS_HASH);
        let testnet = parse_genesis_hash(TESTNET_GENESIS_HASH);
        let localnet = Hash::new_unique();

        validate_rpc_genesis_hash("mainnet-beta", mainnet).unwrap();
        validate_rpc_genesis_hash("devnet", devnet).unwrap();
        validate_rpc_genesis_hash("testnet", testnet).unwrap();
        validate_rpc_genesis_hash("localnet", localnet).unwrap();

        assert!(validate_rpc_genesis_hash("devnet", mainnet).is_err());
        assert!(validate_rpc_genesis_hash("localnet", mainnet).is_err());
        assert!(validate_rpc_genesis_hash("custom", localnet).is_err());
    }
}
