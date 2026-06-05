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
        #[cfg(kani)]
        {
            return Self::read_account_kani(program_id, config_account);
        }

        #[cfg(not(kani))]
        {
            Self::read_account_runtime(program_id, config_account)
        }
    }

    #[cfg(not(kani))]
    fn read_account_runtime(
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

    #[cfg(kani)]
    fn read_account_kani(
        program_id: &Pubkey,
        config_account: &AccountInfo,
    ) -> Result<Self, ProgramError> {
        require_key(config_account, &derive_config(program_id).0)?;
        if !pubkey_eq_kani(config_account.owner(), program_id) {
            return Err(ProgramError::IncorrectProgramId);
        }
        let data = unsafe { config_account.borrow_data_unchecked() };
        if data.len() != HUB_CONFIG_SPACE || !config_magic_eq_kani(data) {
            return Err(ProgramError::InvalidAccountData);
        }

        let admin = read_pubkey_at_unchecked_kani(data, config_account::ADMIN_OFFSET);
        let hub_authorizer =
            read_pubkey_at_unchecked_kani(data, config_account::HUB_AUTHORIZER_OFFSET);
        let inventory_rebalancer =
            read_pubkey_at_unchecked_kani(data, config_account::INVENTORY_REBALANCER_OFFSET);
        let max_fee_bps = read_u16_at_unchecked_kani(data, config_account::MAX_FEE_BPS_OFFSET);
        let paused = data[config_account::PAUSED_OFFSET] != 0;
        let lane_count = data[config_account::LANE_COUNT_OFFSET];
        if lane_count == 0 {
            return Err(ProgramError::InvalidAccountData);
        }
        let mint_count = data[config_account::MINT_COUNT_OFFSET];
        let mint_count_usize = mint_count as usize;
        if mint_count_usize == 0 || mint_count_usize > 2 {
            return Err(ProgramError::InvalidAccountData);
        }

        let mut allowed_mints = [Pubkey::default(); MAX_ALLOWED_MINTS];
        allowed_mints[0] = read_pubkey_at_unchecked_kani(data, config_account::ALLOWED_MINT_OFFSET);
        if mint_count_usize == 2 {
            let second_offset =
                config_account::ALLOWED_MINT_OFFSET + config_account::ALLOWED_MINT_ITEM_LEN;
            allowed_mints[1] = read_pubkey_at_unchecked_kani(data, second_offset);
            if pubkey_eq_kani(&allowed_mints[0], &allowed_mints[1]) {
                return Err(ProgramError::InvalidAccountData);
            }
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

    #[cfg(not(kani))]
    pub fn write_account(&self, config_account: &AccountInfo) -> Result<(), ProgramError> {
        let mut data = config_account.try_borrow_mut_data()?;
        data.fill(0);
        self.write_account_fields(&mut data)
    }

    #[cfg(kani)]
    pub fn write_account(&self, config_account: &AccountInfo) -> Result<(), ProgramError> {
        let data = unsafe { config_account.borrow_mut_data_unchecked() };
        self.write_account_fields_kani(data)
    }

    fn write_account_fields(&self, data: &mut [u8]) -> Result<(), ProgramError> {
        if data.len() != HUB_CONFIG_SPACE {
            return Err(ProgramError::InvalidAccountData);
        }
        write_bytes_at(data, config_account::MAGIC_OFFSET, CONFIG_MAGIC)?;
        write_bytes_at(data, config_account::ADMIN_OFFSET, self.admin.as_ref())?;
        write_bytes_at(
            data,
            config_account::HUB_AUTHORIZER_OFFSET,
            self.hub_authorizer.as_ref(),
        )?;
        write_bytes_at(
            data,
            config_account::INVENTORY_REBALANCER_OFFSET,
            self.inventory_rebalancer.as_ref(),
        )?;
        write_bytes_at(
            data,
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
            write_bytes_at(data, offset, mint.as_ref())?;
        }
        Ok(())
    }

    #[cfg(kani)]
    fn write_account_fields_kani(&self, data: &mut [u8]) -> Result<(), ProgramError> {
        if data.len() != HUB_CONFIG_SPACE {
            return Err(ProgramError::InvalidAccountData);
        }
        write_bytes_at(data, config_account::MAGIC_OFFSET, CONFIG_MAGIC)?;
        write_bytes_at(data, config_account::ADMIN_OFFSET, self.admin.as_ref())?;
        write_bytes_at(
            data,
            config_account::HUB_AUTHORIZER_OFFSET,
            self.hub_authorizer.as_ref(),
        )?;
        write_bytes_at(
            data,
            config_account::INVENTORY_REBALANCER_OFFSET,
            self.inventory_rebalancer.as_ref(),
        )?;
        write_bytes_at(
            data,
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
        write_bytes_at(
            data,
            config_account::ALLOWED_MINT_OFFSET,
            self.allowed_mints[0].as_ref(),
        )?;
        if mint_count == 2 {
            write_bytes_at(
                data,
                config_account::ALLOWED_MINT_OFFSET + config_account::ALLOWED_MINT_ITEM_LEN,
                self.allowed_mints[1].as_ref(),
            )?;
        }
        Ok(())
    }

    pub fn require_allowed_mint(&self, mint: &Pubkey) -> Result<(), ProgramError> {
        let mint_count = self.mint_count as usize;
        if mint_count == 0 || mint_count > MAX_ALLOWED_MINTS {
            return Err(ProgramError::InvalidArgument);
        }
        #[cfg(kani)]
        {
            if pubkey_eq_kani(&self.allowed_mints[0], mint) {
                return Ok(());
            }
            if mint_count == 2 && pubkey_eq_kani(&self.allowed_mints[1], mint) {
                return Ok(());
            }
            return Err(ProgramError::InvalidArgument);
        }

        #[cfg(not(kani))]
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
    let mut key = [0xc0u8; 32];
    key[1] = program_id[0];
    key[2] = CONFIG_SEED[0];
    (key, 255)
}

#[cfg(not(kani))]
pub fn derive_hub_authority(program_id: &Pubkey, lane_id: u8) -> (Pubkey, u8) {
    pinocchio::pubkey::find_program_address(&[HUB_AUTHORITY_SEED, &[lane_id]], program_id)
}

#[cfg(kani)]
pub fn derive_hub_authority(program_id: &Pubkey, lane_id: u8) -> (Pubkey, u8) {
    let mut key = [0xa0u8; 32];
    key[1] = lane_id;
    key[2] = program_id[0];
    key[3] = HUB_AUTHORITY_SEED[0];
    (key, 254)
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
    let mut key = *mint;
    key[0] = 0xb0;
    key[1] = lane_id;
    key[2] = program_id[0];
    key[3] = crate::SPL_TOKEN_ID[0];
    key[4] = ASSOCIATED_TOKEN_PROGRAM_ID[0];
    key
}

#[cfg(kani)]
fn read_pubkey_at_unchecked_kani(data: &[u8], offset: usize) -> Pubkey {
    [
        data[offset],
        data[offset + 1],
        data[offset + 2],
        data[offset + 3],
        data[offset + 4],
        data[offset + 5],
        data[offset + 6],
        data[offset + 7],
        data[offset + 8],
        data[offset + 9],
        data[offset + 10],
        data[offset + 11],
        data[offset + 12],
        data[offset + 13],
        data[offset + 14],
        data[offset + 15],
        data[offset + 16],
        data[offset + 17],
        data[offset + 18],
        data[offset + 19],
        data[offset + 20],
        data[offset + 21],
        data[offset + 22],
        data[offset + 23],
        data[offset + 24],
        data[offset + 25],
        data[offset + 26],
        data[offset + 27],
        data[offset + 28],
        data[offset + 29],
        data[offset + 30],
        data[offset + 31],
    ]
}

#[cfg(kani)]
fn read_u16_at_unchecked_kani(data: &[u8], offset: usize) -> u16 {
    u16::from_le_bytes([data[offset], data[offset + 1]])
}

#[cfg(kani)]
fn pubkey_eq_kani(left: &Pubkey, right: &Pubkey) -> bool {
    pubkey_domain_eq_kani(left, right)
}

#[cfg(kani)]
fn pubkey_domain_eq_kani(left: &Pubkey, right: &Pubkey) -> bool {
    left[0] == right[0]
        && left[1] == right[1]
        && left[2] == right[2]
        && left[3] == right[3]
        && left[4] == right[4]
        && left[5] == right[5]
}

#[cfg(kani)]
fn config_magic_eq_kani(data: &[u8]) -> bool {
    data[config_account::MAGIC_OFFSET] == CONFIG_MAGIC[0]
        && data[config_account::MAGIC_OFFSET + 1] == CONFIG_MAGIC[1]
        && data[config_account::MAGIC_OFFSET + 2] == CONFIG_MAGIC[2]
        && data[config_account::MAGIC_OFFSET + 3] == CONFIG_MAGIC[3]
        && data[config_account::MAGIC_OFFSET + 4] == CONFIG_MAGIC[4]
        && data[config_account::MAGIC_OFFSET + 5] == CONFIG_MAGIC[5]
        && data[config_account::MAGIC_OFFSET + 6] == CONFIG_MAGIC[6]
        && data[config_account::MAGIC_OFFSET + 7] == CONFIG_MAGIC[7]
}
