use std::fmt;

#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct ObservabilityWalletAddress(String);

impl ObservabilityWalletAddress {
    pub fn new(wallet_address: &str) -> Option<Self> {
        let wallet_address = wallet_address.trim();
        if wallet_address.is_empty() {
            return None;
        }
        Some(Self(wallet_address.to_owned()))
    }

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
