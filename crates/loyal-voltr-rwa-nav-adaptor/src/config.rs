use crate::{AdaptorError, AdaptorResult};
use solana_program::pubkey::Pubkey;

pub const CONFIG_VERSION: u8 = 3;
/// Upper bound for `StrategyConfig::max_step_bps`: 100% of the receipt position.
pub const MAX_STEP_BPS_CAP: u64 = 10_000;
pub const CONFIG_DISCRIMINATOR: [u8; 8] = [46, 154, 12, 115, 203, 165, 199, 235];
const PUBKEY_COUNT: usize = 12;
pub const CONFIG_LEN: usize = 16 + PUBKEY_COUNT * 32 + 8 * 5 + 32;
pub const REPORT_V1_LEN: usize = 1 + 8 + 8 + 8 + 32;
pub const REPORT_TICKET_VERSION: u8 = 2;
pub const REPORT_TICKET_DISCRIMINATOR: [u8; 8] = [245, 104, 182, 197, 58, 231, 116, 237];
/// v2 appends `last_consumed_slot: u64` at 96..104 so the report interval can
/// be enforced on the execution slot instead of the observation sequence.
pub const REPORT_TICKET_LEN: usize = 104;

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
pub struct ReportTicket {
    pub bump: u8,
    pub armed: bool,
    pub config: Pubkey,
    pub last_consumed_sequence: u64,
    /// Slot at which `last_consumed_sequence` was consumed; the report interval
    /// is measured against this execution slot.
    pub last_consumed_slot: u64,
    pub active_sequence: u64,
    pub active_wire_sha256: [u8; 32],
}

impl ReportTicket {
    pub fn encode(&self, output: &mut [u8]) -> AdaptorResult<()> {
        if output.len() != REPORT_TICKET_LEN {
            return Err(AdaptorError::InvalidTicket);
        }
        output.fill(0);
        output[..8].copy_from_slice(&REPORT_TICKET_DISCRIMINATOR);
        output[8] = REPORT_TICKET_VERSION;
        output[9] = self.bump;
        output[10] = u8::from(self.armed);
        output[16..48].copy_from_slice(self.config.as_ref());
        output[48..56].copy_from_slice(&self.last_consumed_sequence.to_le_bytes());
        output[56..64].copy_from_slice(&self.active_sequence.to_le_bytes());
        output[64..96].copy_from_slice(&self.active_wire_sha256);
        output[96..104].copy_from_slice(&self.last_consumed_slot.to_le_bytes());
        Ok(())
    }

    pub fn decode(input: &[u8]) -> AdaptorResult<Self> {
        if input.len() != REPORT_TICKET_LEN
            || input[..8] != REPORT_TICKET_DISCRIMINATOR
            || input[8] != REPORT_TICKET_VERSION
            || input[10] > 1
            || input[11..16] != [0; 5]
        {
            return Err(AdaptorError::InvalidTicket);
        }
        Ok(Self {
            bump: input[9],
            armed: input[10] == 1,
            config: Pubkey::new_from_array(
                input[16..48]
                    .try_into()
                    .map_err(|_| AdaptorError::InvalidTicket)?,
            ),
            last_consumed_sequence: u64::from_le_bytes(
                input[48..56]
                    .try_into()
                    .map_err(|_| AdaptorError::InvalidTicket)?,
            ),
            last_consumed_slot: u64::from_le_bytes(
                input[96..104]
                    .try_into()
                    .map_err(|_| AdaptorError::InvalidTicket)?,
            ),
            active_sequence: u64::from_le_bytes(
                input[56..64]
                    .try_into()
                    .map_err(|_| AdaptorError::InvalidTicket)?,
            ),
            active_wire_sha256: input[64..96]
                .try_into()
                .map_err(|_| AdaptorError::InvalidTicket)?,
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
    /// NAV step bound: `step = max(position_value * max_step_bps / 10_000, step_floor_raw)`.
    /// These two fields occupy the u64 slots that v1/v2 reserved as
    /// `last_sequence` / `last_observed_slot`.
    pub max_step_bps: u64,
    pub step_floor_raw: u64,
    /// Minimum slot distance from the last consumed report. A report's sequence
    /// is its observed slot, so this caps how often the reported NAV may move.
    /// Occupies the u64 slot that v1/v2 reserved as `last_nav_raw`. Zero disables.
    pub min_report_interval_slots: u64,
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
            self.max_step_bps,
            self.step_floor_raw,
            self.min_report_interval_slots,
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
        let mut next = || -> AdaptorResult<Pubkey> {
            let value = Pubkey::new_from_array(
                input[offset..offset + 32]
                    .try_into()
                    .map_err(|_| AdaptorError::InvalidConfig)?,
            );
            offset += 32;
            Ok(value)
        };
        let voltr_program = next()?;
        let voltr_vault = next()?;
        let strategy = next()?;
        let vault_strategy_auth = next()?;
        let squads_program = next()?;
        let squads_settings = next()?;
        let squads_settings_signer = next()?;
        let squads_vault = next()?;
        let asset_mint = next()?;
        let asset_token_program = next()?;
        let squads_asset_ata = next()?;
        if next()? != Pubkey::default() {
            return Err(AdaptorError::InvalidConfig);
        }
        let mut next_u64 = || -> AdaptorResult<u64> {
            let value = u64::from_le_bytes(
                input[offset..offset + 8]
                    .try_into()
                    .map_err(|_| AdaptorError::InvalidConfig)?,
            );
            offset += 8;
            Ok(value)
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
            max_report_nav_raw: next_u64()?,
            max_report_age_slots: next_u64()?,
            max_step_bps: next_u64()?,
            step_floor_raw: next_u64()?,
            min_report_interval_slots: next_u64()?,
            last_snapshot_digest: input[offset..offset + 32]
                .try_into()
                .map_err(|_| AdaptorError::InvalidConfig)?,
        })
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

    #[test]
    fn report_ticket_codec_is_exact_and_reserved_bytes_stay_zero() {
        let expected = ReportTicket {
            bump: 254,
            armed: true,
            config: Pubkey::new_unique(),
            last_consumed_sequence: 10,
            last_consumed_slot: 9,
            active_sequence: 11,
            active_wire_sha256: [7; 32],
        };
        let mut bytes = [0; REPORT_TICKET_LEN];
        expected.encode(&mut bytes).unwrap();
        assert_eq!(ReportTicket::decode(&bytes), Ok(expected));
        bytes[11] = 1;
        assert_eq!(
            ReportTicket::decode(&bytes),
            Err(AdaptorError::InvalidTicket)
        );
    }

    fn v3_config() -> StrategyConfig {
        StrategyConfig {
            squads_vault_index: 0,
            voltr_program: Pubkey::new_unique(),
            voltr_vault: Pubkey::new_unique(),
            strategy: Pubkey::new_unique(),
            vault_strategy_auth: Pubkey::new_unique(),
            squads_program: Pubkey::new_unique(),
            squads_settings: Pubkey::new_unique(),
            squads_settings_signer: Pubkey::new_unique(),
            squads_vault: Pubkey::new_unique(),
            asset_mint: Pubkey::new_unique(),
            asset_token_program: Pubkey::new_unique(),
            squads_asset_ata: Pubkey::new_unique(),
            max_report_nav_raw: 2_000_000_000_000,
            max_report_age_slots: 32,
            max_step_bps: 500,
            step_floor_raw: 1_000,
            min_report_interval_slots: 3,
            last_snapshot_digest: [0; 32],
        }
    }

    /// Byte offset of the u64 tail that follows the twelve fixed pubkeys.
    const U64_AREA_START: usize = 16 + PUBKEY_COUNT * 32;

    #[test]
    fn config_v3_codec_round_trips_with_step_params_in_the_former_reserved_slots() {
        let expected = v3_config();
        let mut bytes = [0u8; CONFIG_LEN];
        expected.encode(&mut bytes).unwrap();
        assert_eq!(CONFIG_LEN, 472);
        assert_eq!(bytes[8], 3);
        let offset = U64_AREA_START;
        assert_eq!(
            &bytes[offset..offset + 8],
            &expected.max_report_nav_raw.to_le_bytes()
        );
        assert_eq!(
            &bytes[offset + 8..offset + 16],
            &expected.max_report_age_slots.to_le_bytes()
        );
        assert_eq!(
            &bytes[offset + 16..offset + 24],
            &expected.max_step_bps.to_le_bytes()
        );
        assert_eq!(
            &bytes[offset + 24..offset + 32],
            &expected.step_floor_raw.to_le_bytes()
        );
        assert_eq!(
            &bytes[offset + 32..offset + 40],
            &expected.min_report_interval_slots.to_le_bytes()
        );
        assert_eq!(StrategyConfig::decode(&bytes), Ok(expected));
        // The historical snapshot digest is now the only reserved-zero region.
        for byte in &bytes[U64_AREA_START + 5 * 8..] {
            assert_eq!(*byte, 0);
        }
    }

    #[test]
    fn config_decode_rejects_v2_bytes_and_any_non_v3_version() {
        let mut bytes = [0u8; CONFIG_LEN];
        bytes[..8].copy_from_slice(&CONFIG_DISCRIMINATOR);
        bytes[8] = 2;
        let legacy_offset = U64_AREA_START + 2 * 8;
        // A strategy-one v2 config carried `last_sequence`/`last_observed_slot` here.
        bytes[legacy_offset..legacy_offset + 8].copy_from_slice(&7u64.to_le_bytes());
        bytes[legacy_offset + 8..legacy_offset + 16].copy_from_slice(&88_888u64.to_le_bytes());
        assert_eq!(
            StrategyConfig::decode(&bytes),
            Err(AdaptorError::InvalidConfig)
        );

        let mut v1 = bytes;
        v1[8] = 1;
        assert_eq!(
            StrategyConfig::decode(&v1),
            Err(AdaptorError::InvalidConfig)
        );
    }

    #[test]
    fn config_decode_keeps_rejecting_tampered_reserved_bytes() {
        let expected = v3_config();
        let mut bytes = [0u8; CONFIG_LEN];
        expected.encode(&mut bytes).unwrap();

        let mut wrong_version = bytes;
        wrong_version[8] = 4;
        assert_eq!(
            StrategyConfig::decode(&wrong_version),
            Err(AdaptorError::InvalidConfig)
        );

        let mut reserved_pubkey = bytes;
        reserved_pubkey[16 + 11 * 32] = 1;
        assert_eq!(
            StrategyConfig::decode(&reserved_pubkey),
            Err(AdaptorError::InvalidConfig)
        );

        let mut reserved_header = bytes;
        reserved_header[10] = 1;
        assert_eq!(
            StrategyConfig::decode(&reserved_header),
            Err(AdaptorError::InvalidConfig)
        );

        let short = bytes[..CONFIG_LEN - 1].to_vec();
        assert_eq!(
            StrategyConfig::decode(&short),
            Err(AdaptorError::InvalidConfig)
        );
    }
}
