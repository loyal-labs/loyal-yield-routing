use crate::{AdaptorError, AdaptorResult};
use solana_program::pubkey::Pubkey;

pub const CONFIG_VERSION: u8 = 2;
pub const CONFIG_DISCRIMINATOR: [u8; 8] = [46, 154, 12, 115, 203, 165, 199, 235];
const PUBKEY_COUNT: usize = 12;
pub const CONFIG_LEN: usize = 16 + PUBKEY_COUNT * 32 + 8 * 5 + 32;
pub const REPORT_V1_LEN: usize = 1 + 8 + 8 + 8 + 32;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ReportV1 {
    pub sequence: u64,
    pub observed_slot: u64,
    pub nav_after_raw: u64,
    pub snapshot_digest: [u8; 32],
}
impl ReportV1 {
    pub fn decode(input: &[u8]) -> AdaptorResult<Self> {
        if input.len() != REPORT_V1_LEN || input[0] != 1 {
            return Err(AdaptorError::InvalidReport);
        }
        Ok(Self {
            sequence: u64::from_le_bytes(
                input[1..9]
                    .try_into()
                    .map_err(|_| AdaptorError::InvalidReport)?,
            ),
            observed_slot: u64::from_le_bytes(
                input[9..17]
                    .try_into()
                    .map_err(|_| AdaptorError::InvalidReport)?,
            ),
            nav_after_raw: u64::from_le_bytes(
                input[17..25]
                    .try_into()
                    .map_err(|_| AdaptorError::InvalidReport)?,
            ),
            snapshot_digest: input[25..57]
                .try_into()
                .map_err(|_| AdaptorError::InvalidReport)?,
        })
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct StrategyConfig {
    pub squads_vault_index: u8,
    pub voltr_program: Pubkey,
    pub voltr_vault: Pubkey,
    pub strategy: Pubkey,
    pub vault_strategy_auth: Pubkey,
    pub squads_program: Pubkey,
    pub squads_settings: Pubkey,
    pub squads_settings_signer: Pubkey,
    pub squads_vault: Pubkey,
    pub asset_mint: Pubkey,
    pub asset_token_program: Pubkey,
    pub squads_asset_ata: Pubkey,
    pub max_report_nav_raw: u64,
    pub max_report_age_slots: u64,
    pub last_sequence: u64,
    pub last_observed_slot: u64,
    pub last_nav_raw: u64,
    pub last_snapshot_digest: [u8; 32],
}
impl StrategyConfig {
    pub fn encode(&self, output: &mut [u8]) -> AdaptorResult<()> {
        if output.len() != CONFIG_LEN {
            return Err(AdaptorError::InvalidConfig);
        }
        output.fill(0);
        output[..8].copy_from_slice(&CONFIG_DISCRIMINATOR);
        output[8] = CONFIG_VERSION;
        output[9] = self.squads_vault_index;
        let values = [
            self.voltr_program,
            self.voltr_vault,
            self.strategy,
            self.vault_strategy_auth,
            self.squads_program,
            self.squads_settings,
            self.squads_settings_signer,
            self.squads_vault,
            self.asset_mint,
            self.asset_token_program,
            self.squads_asset_ata,
            Pubkey::default(),
        ];
        for (index, value) in values.iter().enumerate() {
            let start = 16 + index * 32;
            output[start..start + 32].copy_from_slice(value.as_ref());
        }
        let mut offset = 16 + PUBKEY_COUNT * 32;
        for value in [
            self.max_report_nav_raw,
            self.max_report_age_slots,
            self.last_sequence,
            self.last_observed_slot,
            self.last_nav_raw,
        ] {
            output[offset..offset + 8].copy_from_slice(&value.to_le_bytes());
            offset += 8;
        }
        output[offset..offset + 32].copy_from_slice(&self.last_snapshot_digest);
        Ok(())
    }
    pub fn decode(input: &[u8]) -> AdaptorResult<Self> {
        if input.len() != CONFIG_LEN
            || input[..8] != CONFIG_DISCRIMINATOR
            || input[8] != CONFIG_VERSION
            || input[10..16] != [0; 6]
        {
            return Err(AdaptorError::InvalidConfig);
        }
        let mut offset = 16;
        let mut next = || {
            let value = Pubkey::new_from_array(
                input[offset..offset + 32]
                    .try_into()
                    .expect("fixed config width"),
            );
            offset += 32;
            value
        };
        let voltr_program = next();
        let voltr_vault = next();
        let strategy = next();
        let vault_strategy_auth = next();
        let squads_program = next();
        let squads_settings = next();
        let squads_settings_signer = next();
        let squads_vault = next();
        let asset_mint = next();
        let asset_token_program = next();
        let squads_asset_ata = next();
        if next() != Pubkey::default() {
            return Err(AdaptorError::InvalidConfig);
        }
        let mut next_u64 = || {
            let value = u64::from_le_bytes(
                input[offset..offset + 8]
                    .try_into()
                    .expect("fixed config u64 width"),
            );
            offset += 8;
            value
        };
        Ok(Self {
            squads_vault_index: input[9],
            voltr_program,
            voltr_vault,
            strategy,
            vault_strategy_auth,
            squads_program,
            squads_settings,
            squads_settings_signer,
            squads_vault,
            asset_mint,
            asset_token_program,
            squads_asset_ata,
            max_report_nav_raw: next_u64(),
            max_report_age_slots: next_u64(),
            last_sequence: next_u64(),
            last_observed_slot: next_u64(),
            last_nav_raw: next_u64(),
            last_snapshot_digest: input[offset..offset + 32]
                .try_into()
                .map_err(|_| AdaptorError::InvalidConfig)?,
        })
    }
    pub fn accept_report(&mut self, report: ReportV1) {
        self.last_sequence = report.sequence;
        self.last_observed_slot = report.observed_slot;
        self.last_nav_raw = report.nav_after_raw;
        self.last_snapshot_digest = report.snapshot_digest;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn report_v1_rejects_wrong_version_and_trailing_bytes() {
        assert_eq!(
            ReportV1::decode(&[0; REPORT_V1_LEN]),
            Err(AdaptorError::InvalidReport)
        );
        let mut report = vec![1; REPORT_V1_LEN + 1];
        report[0] = 1;
        assert_eq!(ReportV1::decode(&report), Err(AdaptorError::InvalidReport));
    }
}
