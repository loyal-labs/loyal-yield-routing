use solana_program::{account_info::AccountInfo, program_error::ProgramError, pubkey::Pubkey};

use crate::{
    codec::{read_pubkey, read_u16},
    constants::{
        ASSOCIATED_TOKEN_PROGRAM_ID, CONFIG_MAGIC, CONFIG_SEED, HUB_AUTHORITY_SEED,
        HUB_CONFIG_SPACE, MAX_ALLOWED_MINTS,
    },
    validation::require_key,
};

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HubConfig {
    pub admin: Pubkey,
    pub hub_authorizer: Pubkey,
    pub max_fee_bps: u16,
    pub paused: bool,
    pub mint_count: u8,
    pub allowed_mints: [Pubkey; MAX_ALLOWED_MINTS],
}

impl HubConfig {
    pub fn parse(data: &[u8]) -> Result<Self, ProgramError> {
        if data.len() < 68 {
            return Err(ProgramError::InvalidInstructionData);
        }
        let admin = Pubkey::new_from_array(read_pubkey(&data[0..32])?);
        let hub_authorizer = Pubkey::new_from_array(read_pubkey(&data[32..64])?);
        let max_fee_bps = read_u16(&data[64..66])?;
        let paused = data[66] != 0;
        let mint_count = *data.get(67).ok_or(ProgramError::InvalidInstructionData)?;
        let mint_count_usize = mint_count as usize;
        if mint_count_usize == 0 || mint_count_usize > MAX_ALLOWED_MINTS {
            return Err(ProgramError::InvalidInstructionData);
        }

        let expected_len = 68 + (mint_count_usize * 32);
        if data.len() != expected_len {
            return Err(ProgramError::InvalidInstructionData);
        }

        let mut allowed_mints = [Pubkey::default(); MAX_ALLOWED_MINTS];
        for index in 0..mint_count_usize {
            let offset = 68 + (index * 32);
            let mint = Pubkey::new_from_array(read_pubkey(&data[offset..offset + 32])?);
            if allowed_mints[..index].contains(&mint) {
                return Err(ProgramError::InvalidInstructionData);
            }
            allowed_mints[index] = mint;
        }

        Ok(Self {
            admin,
            hub_authorizer,
            max_fee_bps,
            paused,
            mint_count,
            allowed_mints,
        })
    }

    pub fn read_account(
        program_id: &Pubkey,
        config_account: &AccountInfo,
    ) -> Result<Self, ProgramError> {
        require_key(config_account, &derive_config(program_id).0)?;
        if config_account.owner != program_id {
            return Err(ProgramError::IncorrectProgramId);
        }
        let data = config_account.data.borrow();
        if data.len() != HUB_CONFIG_SPACE || &data[..8] != CONFIG_MAGIC {
            return Err(ProgramError::InvalidAccountData);
        }
        let admin = Pubkey::new_from_array(read_pubkey(&data[8..40])?);
        let hub_authorizer = Pubkey::new_from_array(read_pubkey(&data[40..72])?);
        let max_fee_bps = read_u16(&data[72..74])?;
        let paused = data[74] != 0;
        let mint_count = data[75];
        let mint_count_usize = mint_count as usize;
        if mint_count_usize == 0 || mint_count_usize > MAX_ALLOWED_MINTS {
            return Err(ProgramError::InvalidAccountData);
        }

        let mut allowed_mints = [Pubkey::default(); MAX_ALLOWED_MINTS];
        for index in 0..mint_count_usize {
            let offset = 76 + (index * 32);
            let mint = Pubkey::new_from_array(read_pubkey(&data[offset..offset + 32])?);
            if allowed_mints[..index].contains(&mint) {
                return Err(ProgramError::InvalidAccountData);
            }
            allowed_mints[index] = mint;
        }

        Ok(Self {
            admin,
            hub_authorizer,
            max_fee_bps,
            paused,
            mint_count,
            allowed_mints,
        })
    }

    pub fn write_account(&self, config_account: &AccountInfo) -> Result<(), ProgramError> {
        let mut data = config_account.data.borrow_mut();
        if data.len() != HUB_CONFIG_SPACE {
            return Err(ProgramError::InvalidAccountData);
        }
        data.fill(0);
        data[..8].copy_from_slice(CONFIG_MAGIC);
        data[8..40].copy_from_slice(self.admin.as_ref());
        data[40..72].copy_from_slice(self.hub_authorizer.as_ref());
        data[72..74].copy_from_slice(&self.max_fee_bps.to_le_bytes());
        data[74] = u8::from(self.paused);
        let mint_count = self.mint_count as usize;
        if mint_count == 0 || mint_count > MAX_ALLOWED_MINTS {
            return Err(ProgramError::InvalidInstructionData);
        }
        data[75] = self.mint_count;
        for (index, mint) in self.allowed_mints[..mint_count].iter().enumerate() {
            let offset = 76 + (index * 32);
            data[offset..offset + 32].copy_from_slice(mint.as_ref());
        }
        Ok(())
    }

    pub fn require_allowed_mint(&self, mint: &Pubkey) -> Result<(), ProgramError> {
        if !self.allowed_mints[..self.mint_count as usize].contains(mint) {
            return Err(ProgramError::InvalidArgument);
        }
        Ok(())
    }
}

pub fn derive_config(program_id: &Pubkey) -> (Pubkey, u8) {
    Pubkey::find_program_address(&[CONFIG_SEED], program_id)
}

pub fn derive_hub_authority(program_id: &Pubkey) -> (Pubkey, u8) {
    Pubkey::find_program_address(&[HUB_AUTHORITY_SEED], program_id)
}

pub fn derive_inventory_account(program_id: &Pubkey, mint: &Pubkey) -> Pubkey {
    let hub_authority = derive_hub_authority(program_id).0;
    Pubkey::find_program_address(
        &[
            hub_authority.as_ref(),
            spl_token::id().as_ref(),
            mint.as_ref(),
        ],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}
