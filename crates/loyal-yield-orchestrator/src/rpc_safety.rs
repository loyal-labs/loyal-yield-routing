use std::str::FromStr;

use solana_sdk::hash::Hash;

const MAINNET_BETA_GENESIS_HASH: &str = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const DEVNET_GENESIS_HASH: &str = "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG";
const TESTNET_GENESIS_HASH: &str = "4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY";

/// Verifies that an RPC endpoint belongs to the explicitly configured cluster.
///
/// Canonical clusters have fixed genesis hashes. `localnet` accepts only a
/// non-canonical genesis, which prevents a localnet-labelled mutation command
/// from accidentally targeting mainnet, devnet, or testnet.
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

/// Returns only the endpoint origin. User info, path, query, and fragment are
/// deliberately omitted because managed RPC credentials may live in any of
/// those URL components.
pub fn redacted_rpc_endpoint(rpc_url: &str) -> String {
    let Ok(url) = reqwest::Url::parse(rpc_url) else {
        return "<configured>".to_owned();
    };
    if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
        return "<configured>".to_owned();
    }
    url.origin().ascii_serialization()
}

/// Rejects malformed or non-HTTP endpoints without echoing the supplied value.
/// This also ensures fatal-error redaction can recognize every accepted RPC URL
/// by its scheme even when a client library includes it in an error chain.
pub fn validate_rpc_endpoint(rpc_url: &str) -> Result<(), String> {
    let url = reqwest::Url::parse(rpc_url)
        .map_err(|_| "configured RPC endpoint must be an absolute HTTP(S) URL".to_owned())?;
    if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
        return Err("configured RPC endpoint must be an absolute HTTP(S) URL".to_owned());
    }
    Ok(())
}

/// Produces a bounded fatal-error message without endpoint credentials.
///
/// Client error implementations are allowed to render their request URL. An
/// accepted endpoint always contains `://`, so replacing the complete token
/// removes user info, path, query, and fragment together while retaining RPC
/// method/status context around it.
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
                // Redact both the auth scheme and the credential value.
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

    #[test]
    fn reusable_alt_rpc_endpoint_redaction_never_exposes_credentials() {
        assert_eq!(
            redacted_rpc_endpoint("https://user:password@example.test/secret/path?api-key=secret"),
            "https://example.test"
        );
        assert_eq!(
            redacted_rpc_endpoint("https://example.quiknode.pro/path-token/"),
            "https://example.quiknode.pro"
        );
        assert_eq!(
            redacted_rpc_endpoint("http://localhost:8899/private?token=secret"),
            "http://localhost:8899"
        );
        assert_eq!(redacted_rpc_endpoint("not a URL"), "<configured>");
    }

    #[test]
    fn reusable_alt_rpc_endpoint_validation_and_fatal_errors_are_safe() {
        validate_rpc_endpoint("https://user:password@example.test/private?token=secret").unwrap();
        validate_rpc_endpoint("http://localhost:8899").unwrap();
        assert!(validate_rpc_endpoint("example.test/private-token").is_err());
        assert!(validate_rpc_endpoint("file:///tmp/rpc").is_err());

        let safe = redacted_external_error(
            "getLatestBlockhash failed sending https://user:password@example.test/private/path?api-key=secret with HTTP 401 Unauthorized token=also-secret Authorization: Bearer header-secret api-key: separated-secret",
        );
        assert!(safe.contains("getLatestBlockhash"));
        assert!(safe.contains("HTTP 401 Unauthorized"));
        for secret in [
            "user",
            "password",
            "private/path",
            "api-key",
            "secret",
            "also-secret",
            "Bearer",
            "header-secret",
            "separated-secret",
        ] {
            assert!(
                !safe.contains(secret),
                "fatal error leaked {secret}: {safe}"
            );
        }
    }
}
