use pinocchio::{account_info::AccountInfo, program_error::ProgramError, pubkey::Pubkey};

use crate::{
    codec::{read_pubkey_at, read_u16_at, read_u8_at, write_bytes_at},
    constants::{
        ASSOCIATED_TOKEN_PROGRAM_ID, CONFIG_MAGIC, CONFIG_SEED, HUB_AUTHORITY_SEED,
        HUB_CONFIG_SPACE, MAX_ALLOWED_MINTS,
    },
    validation::require_key,
};
use loyal_hub_abi::{config_account, config_init};

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HubConfig {
    pub admin: Pubkey,
    pub hub_authorizer: Pubkey,
    pub inventory_rebalancer: Pubkey,
    pub max_fee_bps: u16,
    pub paused: bool,
    pub lane_count: u8,
    pub mint_count: u8,
    pub allowed_mints: [Pubkey; MAX_ALLOWED_MINTS],
}

impl HubConfig {
    pub fn parse(data: &[u8]) -> Result<Self, ProgramError> {
        if data.len() < config_init::FIXED_LEN {
            return Err(ProgramError::InvalidInstructionData);
        }
        let admin = read_pubkey_at(data, config_init::ADMIN_OFFSET)?;
        let hub_authorizer = read_pubkey_at(data, config_init::HUB_AUTHORIZER_OFFSET)?;
        let inventory_rebalancer = read_pubkey_at(data, config_init::INVENTORY_REBALANCER_OFFSET)?;
        let max_fee_bps = read_u16_at(data, config_init::MAX_FEE_BPS_OFFSET)?;
        let paused = read_u8_at(data, config_init::PAUSED_OFFSET)? != 0;
        let lane_count = read_u8_at(data, config_init::LANE_COUNT_OFFSET)?;
        if lane_count == 0 {
            return Err(ProgramError::InvalidInstructionData);
        }
        let mint_count = read_u8_at(data, config_init::MINT_COUNT_OFFSET)?;
        let mint_count_usize = mint_count as usize;
        if mint_count_usize == 0 || mint_count_usize > MAX_ALLOWED_MINTS {
            return Err(ProgramError::InvalidInstructionData);
        }

        let expected_len =
            config_init::FIXED_LEN + (mint_count_usize * config_init::ALLOWED_MINT_ITEM_LEN);
        if data.len() != expected_len {
            return Err(ProgramError::InvalidInstructionData);
        }

        let mut allowed_mints = [Pubkey::default(); MAX_ALLOWED_MINTS];
        for index in 0..mint_count_usize {
            let offset = config_init::ALLOWED_MINT_OFFSET
                .checked_add(
                    index
                        .checked_mul(config_init::ALLOWED_MINT_ITEM_LEN)
                        .ok_or(ProgramError::InvalidInstructionData)?,
                )
                .ok_or(ProgramError::InvalidInstructionData)?;
            let mint = read_pubkey_at(data, offset)?;
            if allowed_mints[..index].contains(&mint) {
                return Err(ProgramError::InvalidInstructionData);
            }
            allowed_mints[index] = mint;
        }

        Ok(Self {
            admin,
            hub_authorizer,
            inventory_rebalancer,
            max_fee_bps,
            paused,
            lane_count,
            mint_count,
            allowed_mints,
        })
    }

    pub fn read_account(
        program_id: &Pubkey,
        config_account: &AccountInfo,
    ) -> Result<Self, ProgramError> {
        require_key(config_account, &derive_config(program_id).0)?;
        if config_account.owner() != program_id {
            return Err(ProgramError::IncorrectProgramId);
        }
        let data = config_account.try_borrow_data()?;
        if data.len() != HUB_CONFIG_SPACE
            || data.get(
                config_account::MAGIC_OFFSET
                    ..config_account::MAGIC_OFFSET + config_account::MAGIC_LEN,
            ) != Some(CONFIG_MAGIC.as_slice())
        {
            return Err(ProgramError::InvalidAccountData);
        }
        let admin = read_pubkey_at(&data, config_account::ADMIN_OFFSET)?;
        let hub_authorizer = read_pubkey_at(&data, config_account::HUB_AUTHORIZER_OFFSET)?;
        let inventory_rebalancer =
            read_pubkey_at(&data, config_account::INVENTORY_REBALANCER_OFFSET)?;
        let max_fee_bps = read_u16_at(&data, config_account::MAX_FEE_BPS_OFFSET)?;
        let paused = read_u8_at(&data, config_account::PAUSED_OFFSET)? != 0;
        let lane_count = read_u8_at(&data, config_account::LANE_COUNT_OFFSET)?;
        if lane_count == 0 {
            return Err(ProgramError::InvalidAccountData);
        }
        let mint_count = read_u8_at(&data, config_account::MINT_COUNT_OFFSET)?;
        let mint_count_usize = mint_count as usize;
        if mint_count_usize == 0 || mint_count_usize > MAX_ALLOWED_MINTS {
            return Err(ProgramError::InvalidAccountData);
        }

        let mut allowed_mints = [Pubkey::default(); MAX_ALLOWED_MINTS];
        for index in 0..mint_count_usize {
            let offset = config_account::ALLOWED_MINT_OFFSET
                .checked_add(
                    index
                        .checked_mul(config_account::ALLOWED_MINT_ITEM_LEN)
                        .ok_or(ProgramError::InvalidAccountData)?,
                )
                .ok_or(ProgramError::InvalidAccountData)?;
            let mint =
                read_pubkey_at(&data, offset).map_err(|_| ProgramError::InvalidAccountData)?;
            if allowed_mints[..index].contains(&mint) {
                return Err(ProgramError::InvalidAccountData);
            }
            allowed_mints[index] = mint;
        }

        Ok(Self {
            admin,
            hub_authorizer,
            inventory_rebalancer,
            max_fee_bps,
            paused,
            lane_count,
            mint_count,
            allowed_mints,
        })
    }

    pub fn write_account(&self, config_account: &AccountInfo) -> Result<(), ProgramError> {
        let mut data = config_account.try_borrow_mut_data()?;
        if data.len() != HUB_CONFIG_SPACE {
            return Err(ProgramError::InvalidAccountData);
        }
        data.fill(0);
        write_bytes_at(&mut data, config_account::MAGIC_OFFSET, CONFIG_MAGIC)?;
        write_bytes_at(&mut data, config_account::ADMIN_OFFSET, self.admin.as_ref())?;
        write_bytes_at(
            &mut data,
            config_account::HUB_AUTHORIZER_OFFSET,
            self.hub_authorizer.as_ref(),
        )?;
        write_bytes_at(
            &mut data,
            config_account::INVENTORY_REBALANCER_OFFSET,
            self.inventory_rebalancer.as_ref(),
        )?;
        write_bytes_at(
            &mut data,
            config_account::MAX_FEE_BPS_OFFSET,
            &self.max_fee_bps.to_le_bytes(),
        )?;
        data[config_account::PAUSED_OFFSET] = u8::from(self.paused);
        if self.lane_count == 0 {
            return Err(ProgramError::InvalidInstructionData);
        }
        data[config_account::LANE_COUNT_OFFSET] = self.lane_count;
        let mint_count = self.mint_count as usize;
        if mint_count == 0 || mint_count > MAX_ALLOWED_MINTS {
            return Err(ProgramError::InvalidInstructionData);
        }
        data[config_account::MINT_COUNT_OFFSET] = self.mint_count;
        for (index, mint) in self.allowed_mints[..mint_count].iter().enumerate() {
            let offset = config_account::ALLOWED_MINT_OFFSET
                + (index * config_account::ALLOWED_MINT_ITEM_LEN);
            write_bytes_at(&mut data, offset, mint.as_ref())?;
        }
        Ok(())
    }

    pub fn require_allowed_mint(&self, mint: &Pubkey) -> Result<(), ProgramError> {
        let mint_count = self.mint_count as usize;
        if mint_count == 0 || mint_count > MAX_ALLOWED_MINTS {
            return Err(ProgramError::InvalidArgument);
        }
        if !self.allowed_mints[..mint_count].contains(mint) {
            return Err(ProgramError::InvalidArgument);
        }
        Ok(())
    }

    pub fn require_lane(&self, lane_id: u8) -> Result<(), ProgramError> {
        if self.lane_count == 0 || lane_id >= self.lane_count {
            return Err(ProgramError::InvalidArgument);
        }
        Ok(())
    }
}

#[cfg(not(kani))]
pub fn derive_config(program_id: &Pubkey) -> (Pubkey, u8) {
    pinocchio::pubkey::find_program_address(&[CONFIG_SEED], program_id)
}

#[cfg(kani)]
pub fn derive_config(program_id: &Pubkey) -> (Pubkey, u8) {
    let derived = pinocchio::pubkey::try_find_program_address(&[CONFIG_SEED], program_id);
    kani::assume(derived.is_some());
    derived.unwrap()
}

#[cfg(not(kani))]
pub fn derive_hub_authority(program_id: &Pubkey, lane_id: u8) -> (Pubkey, u8) {
    pinocchio::pubkey::find_program_address(&[HUB_AUTHORITY_SEED, &[lane_id]], program_id)
}

#[cfg(kani)]
pub fn derive_hub_authority(program_id: &Pubkey, lane_id: u8) -> (Pubkey, u8) {
    let derived =
        pinocchio::pubkey::try_find_program_address(&[HUB_AUTHORITY_SEED, &[lane_id]], program_id);
    kani::assume(derived.is_some());
    derived.unwrap()
}

#[cfg(not(kani))]
pub fn derive_inventory_account(program_id: &Pubkey, mint: &Pubkey, lane_id: u8) -> Pubkey {
    let hub_authority = derive_hub_authority(program_id, lane_id).0;
    pinocchio::pubkey::find_program_address(
        &[
            hub_authority.as_ref(),
            crate::SPL_TOKEN_ID.as_ref(),
            mint.as_ref(),
        ],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

#[cfg(kani)]
pub fn derive_inventory_account(program_id: &Pubkey, mint: &Pubkey, lane_id: u8) -> Pubkey {
    let hub_authority = derive_hub_authority(program_id, lane_id).0;
    let derived = pinocchio::pubkey::try_find_program_address(
        &[
            hub_authority.as_ref(),
            crate::SPL_TOKEN_ID.as_ref(),
            mint.as_ref(),
        ],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    );
    kani::assume(derived.is_some());
    derived.unwrap().0
}
