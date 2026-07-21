//! Wallet addresses attached to operational events.

use std::fmt;

/// A wallet address exported verbatim on observability events.
///
/// This is the raw on-chain address, not a pseudonym. It is exported as
/// `loyal.wallet.address` and is directly linkable to on-chain activity, so
/// treat anything carrying it as sensitive operator-only telemetry.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct ObservabilityWalletAddress(String);

impl ObservabilityWalletAddress {
    /// Creates a wallet address, rejecting empty or whitespace-only input.
    ///
    /// Only surrounding whitespace is removed. Case is preserved because
    /// Solana base58 addresses are case-sensitive.
    pub fn new(wallet_address: &str) -> Option<Self> {
        let wallet_address = wallet_address.trim();
        if wallet_address.is_empty() {
            return None;
        }
        Some(Self(wallet_address.to_owned()))
    }

    /// Returns the wallet address.
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl fmt::Display for ObservabilityWalletAddress {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

impl AsRef<str> for ObservabilityWalletAddress {
    fn as_ref(&self) -> &str {
        self.as_str()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn preserves_address_verbatim() {
        let wallet = ObservabilityWalletAddress::new("  11111111111111111111111111111111  ")
            .expect("a non-empty address should be accepted");

        assert_eq!(wallet.as_str(), "11111111111111111111111111111111");
    }

    #[test]
    fn preserves_base58_case() {
        let wallet =
            ObservabilityWalletAddress::new("9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin")
                .expect("a non-empty address should be accepted");

        assert_eq!(
            wallet.as_str(),
            "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"
        );
    }

    #[test]
    fn rejects_empty_input() {
        assert!(ObservabilityWalletAddress::new("").is_none());
        assert!(ObservabilityWalletAddress::new("   ").is_none());
    }
}
