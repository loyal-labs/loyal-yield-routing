#[allow(deprecated)]
use solana_sdk::system_program;
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};
use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
};

const MANIFEST_HASH_DOMAIN: &[u8] = b"loyal-route-alt-manifest-v1";
const SHARED_MARKET_HASH_DOMAIN: &[u8] = b"loyal-route-alt-shared-market-v1";
const VAULT_HASH_DOMAIN: &[u8] = b"loyal-route-alt-vault-v1";
const ADVANCE_NONCE_PREFIX: [u8; 4] = [4, 0, 0, 0];

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum LookupTableAccountAccess {
    Readonly,
    Writable,
}

impl LookupTableAccountAccess {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Readonly => "readonly",
            Self::Writable => "writable",
        }
    }

    fn from_writable(is_writable: bool) -> Self {
        if is_writable {
            Self::Writable
        } else {
            Self::Readonly
        }
    }

    fn hash_tag(self) -> u8 {
        match self {
            Self::Readonly => 0,
            Self::Writable => 1,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum MustRemainStaticReason {
    FeePayer,
    Signer,
    Nonce,
    InvokedProgram,
}

impl MustRemainStaticReason {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::FeePayer => "fee_payer",
            Self::Signer => "signer",
            Self::Nonce => "nonce",
            Self::InvokedProgram => "invoked_program",
        }
    }

    fn hash_tag(self) -> u8 {
        match self {
            Self::FeePayer => 0,
            Self::Signer => 1,
            Self::Nonce => 2,
            Self::InvokedProgram => 3,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum SharedMarketRole {
    Market,
    MarketAuthority,
    Reserve,
    LiquidityMint,
    LiquiditySupply,
    CollateralMint,
    CollateralSupply,
    Oracle,
    ScopePrices,
    ReserveFarmState,
    Infrastructure,
}

impl SharedMarketRole {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Market => "market",
            Self::MarketAuthority => "market_authority",
            Self::Reserve => "reserve",
            Self::LiquidityMint => "liquidity_mint",
            Self::LiquiditySupply => "liquidity_supply",
            Self::CollateralMint => "collateral_mint",
            Self::CollateralSupply => "collateral_supply",
            Self::Oracle => "oracle",
            Self::ScopePrices => "scope_prices",
            Self::ReserveFarmState => "reserve_farm_state",
            Self::Infrastructure => "infrastructure",
        }
    }

    fn hash_tag(self) -> u8 {
        match self {
            Self::Market => 0,
            Self::MarketAuthority => 1,
            Self::Reserve => 2,
            Self::LiquidityMint => 3,
            Self::LiquiditySupply => 4,
            Self::CollateralMint => 5,
            Self::CollateralSupply => 6,
            Self::Oracle => 7,
            Self::ScopePrices => 8,
            Self::ReserveFarmState => 9,
            Self::Infrastructure => 10,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum VaultRole {
    Settings,
    Vault,
    Obligation,
    Policy,
    ActionAccount,
    VaultTokenAccount,
    Metadata,
    FarmUserState,
}

impl VaultRole {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Settings => "settings",
            Self::Vault => "vault",
            Self::Obligation => "obligation",
            Self::Policy => "policy",
            Self::ActionAccount => "action_account",
            Self::VaultTokenAccount => "vault_token_account",
            Self::Metadata => "metadata",
            Self::FarmUserState => "farm_user_state",
        }
    }

    fn hash_tag(self) -> u8 {
        match self {
            Self::Settings => 0,
            Self::Vault => 1,
            Self::Obligation => 2,
            Self::Policy => 3,
            Self::ActionAccount => 4,
            Self::VaultTokenAccount => 5,
            Self::Metadata => 6,
            Self::FarmUserState => 7,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MustRemainStatic {
    pub address: Pubkey,
    pub access: LookupTableAccountAccess,
    pub reasons: BTreeSet<MustRemainStaticReason>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SharedMarket {
    pub address: Pubkey,
    pub access: LookupTableAccountAccess,
    pub roles: BTreeSet<SharedMarketRole>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Vault {
    pub address: Pubkey,
    pub access: LookupTableAccountAccess,
    pub roles: BTreeSet<VaultRole>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct LookupTableAccountProvenance {
    shared_market: BTreeMap<Pubkey, BTreeSet<SharedMarketRole>>,
    vault: BTreeMap<Pubkey, BTreeSet<VaultRole>>,
}

impl LookupTableAccountProvenance {
    pub fn add_shared_market(&mut self, address: Pubkey, role: SharedMarketRole) {
        self.shared_market.entry(address).or_default().insert(role);
    }

    pub fn add_vault(&mut self, address: Pubkey, role: VaultRole) {
        self.vault.entry(address).or_default().insert(role);
    }

    pub fn shared_market_roles(&self, address: &Pubkey) -> Option<&BTreeSet<SharedMarketRole>> {
        self.shared_market.get(address)
    }

    pub fn vault_roles(&self, address: &Pubkey) -> Option<&BTreeSet<VaultRole>> {
        self.vault.get(address)
    }

    pub fn merge(&mut self, other: &Self) -> Result<(), LookupTableManifestError> {
        if let Some(address) = self
            .shared_market
            .keys()
            .find(|address| self.vault.contains_key(address))
            .or_else(|| {
                self.shared_market
                    .keys()
                    .find(|address| other.vault.contains_key(address))
            })
            .or_else(|| {
                other
                    .shared_market
                    .keys()
                    .find(|address| self.vault.contains_key(address))
            })
            .or_else(|| {
                other
                    .shared_market
                    .keys()
                    .find(|address| other.vault.contains_key(address))
            })
        {
            return Err(LookupTableManifestError::ConflictingProvenance { address: *address });
        }

        for (address, roles) in &other.shared_market {
            self.shared_market
                .entry(*address)
                .or_default()
                .extend(roles);
        }
        for (address, roles) in &other.vault {
            self.vault.entry(*address).or_default().extend(roles);
        }
        Ok(())
    }
}

/// Semantic Kamino accounts owned by a route phase. Callers provide accounts;
/// this type, rather than the runtime or test fixture, owns their ALT class and
/// role mapping.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct KaminoReserveLookupTableAccounts {
    pub market: Pubkey,
    pub market_authorities: Vec<Pubkey>,
    pub reserve: Pubkey,
    pub liquidity_mint: Pubkey,
    pub liquidity_supply: Option<Pubkey>,
    pub collateral_mint: Option<Pubkey>,
    pub collateral_supply: Option<Pubkey>,
    pub oracles: Vec<Pubkey>,
    pub scope_prices: Option<Pubkey>,
    pub reserve_farm_state: Option<Pubkey>,
    pub obligation_reserves: Vec<Pubkey>,
    pub infrastructure: Vec<Pubkey>,
}

impl KaminoReserveLookupTableAccounts {
    pub fn new(market: Pubkey, reserve: Pubkey, liquidity_mint: Pubkey) -> Self {
        Self {
            market,
            market_authorities: Vec::new(),
            reserve,
            liquidity_mint,
            liquidity_supply: None,
            collateral_mint: None,
            collateral_supply: None,
            oracles: Vec::new(),
            scope_prices: None,
            reserve_farm_state: None,
            obligation_reserves: Vec::new(),
            infrastructure: Vec::new(),
        }
    }
}

/// Builder-owned semantic requirements for a yield-route transaction or
/// phase. The raw provenance maps stay encapsulated so consumers cannot drift
/// into duplicating the shared-market versus vault role policy.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct YieldRouteLookupTableRequirements {
    provenance: LookupTableAccountProvenance,
}

impl YieldRouteLookupTableRequirements {
    pub fn new(settings: Pubkey, vault: Pubkey) -> Self {
        let mut requirements = Self::default();
        requirements.add_settings(settings);
        requirements.add_vault_account(vault);
        requirements
    }

    pub fn provenance(&self) -> &LookupTableAccountProvenance {
        &self.provenance
    }

    pub fn into_provenance(self) -> LookupTableAccountProvenance {
        self.provenance
    }

    pub fn merge(&mut self, other: &Self) -> Result<(), LookupTableManifestError> {
        self.provenance.merge(&other.provenance)
    }

    pub fn manifest(
        &self,
        payer: Pubkey,
        instructions: &[Instruction],
    ) -> Result<LookupTableManifest, LookupTableManifestError> {
        LookupTableManifest::from_instructions(payer, instructions, &self.provenance)
    }

    pub fn add_settings(&mut self, address: Pubkey) {
        self.provenance.add_vault(address, VaultRole::Settings);
    }

    pub fn add_vault_account(&mut self, address: Pubkey) {
        self.provenance.add_vault(address, VaultRole::Vault);
    }

    pub fn add_obligation(&mut self, address: Pubkey) {
        self.provenance.add_vault(address, VaultRole::Obligation);
    }

    pub fn add_policy(&mut self, address: Pubkey) {
        self.provenance.add_vault(address, VaultRole::Policy);
    }

    pub fn add_action_account(&mut self, address: Pubkey) {
        self.provenance.add_vault(address, VaultRole::ActionAccount);
    }

    pub fn add_vault_token_account(&mut self, address: Pubkey) {
        self.provenance
            .add_vault(address, VaultRole::VaultTokenAccount);
    }

    pub fn add_metadata(&mut self, address: Pubkey) {
        self.provenance.add_vault(address, VaultRole::Metadata);
    }

    pub fn add_farm_user_state(&mut self, address: Pubkey) {
        self.provenance.add_vault(address, VaultRole::FarmUserState);
    }

    pub fn add_kamino_farm(&mut self, reserve_farm_state: Pubkey, farm_user_state: Pubkey) {
        self.provenance
            .add_shared_market(reserve_farm_state, SharedMarketRole::ReserveFarmState);
        self.add_farm_user_state(farm_user_state);
    }

    pub fn add_infrastructure(&mut self, address: Pubkey) {
        self.provenance
            .add_shared_market(address, SharedMarketRole::Infrastructure);
    }

    pub fn add_infrastructure_accounts(&mut self, addresses: impl IntoIterator<Item = Pubkey>) {
        for address in addresses {
            self.add_infrastructure(address);
        }
    }

    pub fn add_shared_reserve(&mut self, address: Pubkey) {
        self.provenance
            .add_shared_market(address, SharedMarketRole::Reserve);
    }

    pub fn add_shared_market(&mut self, address: Pubkey) {
        self.provenance
            .add_shared_market(address, SharedMarketRole::Market);
    }

    pub fn add_shared_market_authority(&mut self, address: Pubkey) {
        self.provenance
            .add_shared_market(address, SharedMarketRole::MarketAuthority);
    }

    pub fn add_shared_liquidity_mint(&mut self, address: Pubkey) {
        self.provenance
            .add_shared_market(address, SharedMarketRole::LiquidityMint);
    }

    pub fn add_kamino_reserve(&mut self, accounts: KaminoReserveLookupTableAccounts) {
        self.provenance
            .add_shared_market(accounts.market, SharedMarketRole::Market);
        for authority in accounts.market_authorities {
            self.provenance
                .add_shared_market(authority, SharedMarketRole::MarketAuthority);
        }
        self.provenance
            .add_shared_market(accounts.reserve, SharedMarketRole::Reserve);
        self.provenance
            .add_shared_market(accounts.liquidity_mint, SharedMarketRole::LiquidityMint);
        if let Some(address) = accounts.liquidity_supply {
            self.provenance
                .add_shared_market(address, SharedMarketRole::LiquiditySupply);
        }
        if let Some(address) = accounts.collateral_mint {
            self.provenance
                .add_shared_market(address, SharedMarketRole::CollateralMint);
        }
        if let Some(address) = accounts.collateral_supply {
            self.provenance
                .add_shared_market(address, SharedMarketRole::CollateralSupply);
        }
        for oracle in accounts.oracles {
            self.provenance
                .add_shared_market(oracle, SharedMarketRole::Oracle);
        }
        if let Some(address) = accounts.scope_prices {
            self.provenance
                .add_shared_market(address, SharedMarketRole::ScopePrices);
        }
        if let Some(address) = accounts.reserve_farm_state {
            self.provenance
                .add_shared_market(address, SharedMarketRole::ReserveFarmState);
        }
        for reserve in accounts.obligation_reserves {
            self.add_shared_reserve(reserve);
        }
        self.add_infrastructure_accounts(accounts.infrastructure);
    }
}

/// An instruction paired with the semantic ALT requirements emitted by the
/// builder that produced it. Wrapping or compiling the instruction may change
/// its outer shape, but must carry these requirements forward.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct YieldRouteInstruction {
    instruction: Instruction,
    lookup_table_requirements: YieldRouteLookupTableRequirements,
}

impl YieldRouteInstruction {
    pub fn new(
        instruction: Instruction,
        lookup_table_requirements: YieldRouteLookupTableRequirements,
    ) -> Self {
        Self {
            instruction,
            lookup_table_requirements,
        }
    }

    pub fn instruction(&self) -> &Instruction {
        &self.instruction
    }

    pub fn lookup_table_requirements(&self) -> &YieldRouteLookupTableRequirements {
        &self.lookup_table_requirements
    }

    pub fn into_parts(self) -> (Instruction, YieldRouteLookupTableRequirements) {
        (self.instruction, self.lookup_table_requirements)
    }
}

/// Ordered transaction instructions with requirements composed before the
/// instruction vector is flattened for v0 compilation.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct YieldRouteInstructionPlan {
    instructions: Vec<Instruction>,
    lookup_table_requirements: YieldRouteLookupTableRequirements,
}

impl YieldRouteInstructionPlan {
    pub fn with_outer_context(
        lookup_table_requirements: YieldRouteLookupTableRequirements,
    ) -> Self {
        Self {
            instructions: Vec::new(),
            lookup_table_requirements,
        }
    }

    pub fn push(
        &mut self,
        routed_instruction: YieldRouteInstruction,
    ) -> Result<(), LookupTableManifestError> {
        let (instruction, requirements) = routed_instruction.into_parts();
        self.lookup_table_requirements.merge(&requirements)?;
        self.instructions.push(instruction);
        Ok(())
    }

    /// Adds an instruction whose only non-static accounts are already present
    /// in the explicit outer context (for example, a payer-to-vault transfer).
    pub fn push_outer_instruction(&mut self, instruction: Instruction) {
        self.instructions.push(instruction);
    }

    pub fn instructions(&self) -> &[Instruction] {
        &self.instructions
    }

    pub fn lookup_table_requirements(&self) -> &YieldRouteLookupTableRequirements {
        &self.lookup_table_requirements
    }

    pub fn manifest(&self, payer: Pubkey) -> Result<LookupTableManifest, LookupTableManifestError> {
        self.lookup_table_requirements
            .manifest(payer, &self.instructions)
    }

    pub fn into_parts(self) -> (Vec<Instruction>, YieldRouteLookupTableRequirements) {
        (self.instructions, self.lookup_table_requirements)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LookupTableManifest {
    must_remain_static: Vec<MustRemainStatic>,
    shared_market: Vec<SharedMarket>,
    vault: Vec<Vault>,
}

impl LookupTableManifest {
    pub fn from_instructions(
        payer: Pubkey,
        instructions: &[Instruction],
        provenance: &LookupTableAccountProvenance,
    ) -> Result<Self, LookupTableManifestError> {
        if let Some(address) = provenance
            .shared_market
            .keys()
            .find(|address| provenance.vault.contains_key(address))
        {
            return Err(LookupTableManifestError::ConflictingProvenance { address: *address });
        }

        let compiler_accounts = compiler_accounts(payer, instructions);
        let mut must_remain_static = Vec::new();
        let mut shared_market = Vec::new();
        let mut vault = Vec::new();

        for (address, account) in compiler_accounts {
            let access = LookupTableAccountAccess::from_writable(account.is_writable);
            let reasons = account.static_reasons(address, payer);
            if !reasons.is_empty() {
                must_remain_static.push(MustRemainStatic {
                    address,
                    access,
                    reasons,
                });
                continue;
            }

            if let Some(roles) = provenance.shared_market.get(&address) {
                shared_market.push(SharedMarket {
                    address,
                    access,
                    roles: roles.clone(),
                });
            } else if let Some(roles) = provenance.vault.get(&address) {
                vault.push(Vault {
                    address,
                    access,
                    roles: roles.clone(),
                });
            } else {
                return Err(LookupTableManifestError::MissingProvenance { address });
            }
        }

        let manifest = Self {
            must_remain_static,
            shared_market,
            vault,
        };
        manifest.validate_against_instructions(payer, instructions)?;
        Ok(manifest)
    }

    pub fn must_remain_static(&self) -> &[MustRemainStatic] {
        &self.must_remain_static
    }

    pub fn shared_market(&self) -> &[SharedMarket] {
        &self.shared_market
    }

    pub fn vault(&self) -> &[Vault] {
        &self.vault
    }

    pub fn lookup_eligible_addresses(&self) -> Vec<Pubkey> {
        let mut addresses = self
            .shared_market
            .iter()
            .map(|requirement| requirement.address)
            .chain(self.vault.iter().map(|requirement| requirement.address))
            .collect::<Vec<_>>();
        addresses.sort();
        addresses
    }

    pub fn canonical_hash_input(&self) -> Vec<u8> {
        let mut output = hash_input_prefix(MANIFEST_HASH_DOMAIN);
        append_static_requirements(&mut output, &self.must_remain_static);
        append_shared_market_requirements(&mut output, &self.shared_market);
        append_vault_requirements(&mut output, &self.vault);
        output
    }

    pub fn shared_market_hash_input(&self) -> Vec<u8> {
        let mut output = hash_input_prefix(SHARED_MARKET_HASH_DOMAIN);
        append_shared_market_requirements(&mut output, &self.shared_market);
        output
    }

    pub fn vault_hash_input(&self) -> Vec<u8> {
        let mut output = hash_input_prefix(VAULT_HASH_DOMAIN);
        append_vault_requirements(&mut output, &self.vault);
        output
    }

    pub fn validate_against_instructions(
        &self,
        payer: Pubkey,
        instructions: &[Instruction],
    ) -> Result<(), LookupTableManifestError> {
        let compiler_accounts = compiler_accounts(payer, instructions);
        let static_addresses = self
            .must_remain_static
            .iter()
            .map(|requirement| requirement.address)
            .collect::<BTreeSet<_>>();
        let shared_addresses = self
            .shared_market
            .iter()
            .map(|requirement| requirement.address)
            .collect::<BTreeSet<_>>();
        let vault_addresses = self
            .vault
            .iter()
            .map(|requirement| requirement.address)
            .collect::<BTreeSet<_>>();

        if let Some(address) = static_addresses.intersection(&shared_addresses).next() {
            return Err(LookupTableManifestError::OverlappingManifest { address: *address });
        }
        if let Some(address) = static_addresses.intersection(&vault_addresses).next() {
            return Err(LookupTableManifestError::OverlappingManifest { address: *address });
        }
        if let Some(address) = shared_addresses.intersection(&vault_addresses).next() {
            return Err(LookupTableManifestError::OverlappingManifest { address: *address });
        }

        let manifest_universe = static_addresses
            .iter()
            .chain(shared_addresses.iter())
            .chain(vault_addresses.iter())
            .copied()
            .collect::<BTreeSet<_>>();
        let compiler_universe = compiler_accounts.keys().copied().collect::<BTreeSet<_>>();
        if manifest_universe != compiler_universe {
            return Err(LookupTableManifestError::IncompleteManifest {
                missing: compiler_universe
                    .difference(&manifest_universe)
                    .copied()
                    .collect(),
                extra: manifest_universe
                    .difference(&compiler_universe)
                    .copied()
                    .collect(),
            });
        }

        for requirement in &self.must_remain_static {
            let compiler = &compiler_accounts[&requirement.address];
            let expected_reasons = compiler.static_reasons(requirement.address, payer);
            if expected_reasons != requirement.reasons {
                return Err(LookupTableManifestError::StaticReasonMismatch {
                    address: requirement.address,
                });
            }
            validate_access(
                requirement.address,
                requirement.access,
                compiler.is_writable,
            )?;
        }
        for requirement in &self.shared_market {
            let compiler = &compiler_accounts[&requirement.address];
            if !compiler
                .static_reasons(requirement.address, payer)
                .is_empty()
            {
                return Err(LookupTableManifestError::StaticAccountInLookupManifest {
                    address: requirement.address,
                });
            }
            validate_access(
                requirement.address,
                requirement.access,
                compiler.is_writable,
            )?;
        }
        for requirement in &self.vault {
            let compiler = &compiler_accounts[&requirement.address];
            if !compiler
                .static_reasons(requirement.address, payer)
                .is_empty()
            {
                return Err(LookupTableManifestError::StaticAccountInLookupManifest {
                    address: requirement.address,
                });
            }
            validate_access(
                requirement.address,
                requirement.access,
                compiler.is_writable,
            )?;
        }

        Ok(())
    }
}

pub fn compiler_lookup_eligible_addresses(
    payer: Pubkey,
    instructions: &[Instruction],
) -> Vec<Pubkey> {
    compiler_accounts(payer, instructions)
        .into_iter()
        .filter_map(|(address, account)| {
            account
                .static_reasons(address, payer)
                .is_empty()
                .then_some(address)
        })
        .collect()
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct CompilerAccount {
    is_signer: bool,
    is_writable: bool,
    is_invoked: bool,
    is_nonce: bool,
}

impl CompilerAccount {
    fn static_reasons(self, address: Pubkey, payer: Pubkey) -> BTreeSet<MustRemainStaticReason> {
        let mut reasons = BTreeSet::new();
        if address == payer {
            reasons.insert(MustRemainStaticReason::FeePayer);
        }
        if self.is_signer {
            reasons.insert(MustRemainStaticReason::Signer);
        }
        if self.is_nonce {
            reasons.insert(MustRemainStaticReason::Nonce);
        }
        if self.is_invoked {
            reasons.insert(MustRemainStaticReason::InvokedProgram);
        }
        reasons
    }
}

fn compiler_accounts(
    payer: Pubkey,
    instructions: &[Instruction],
) -> BTreeMap<Pubkey, CompilerAccount> {
    let mut accounts = BTreeMap::<Pubkey, CompilerAccount>::new();
    for instruction in instructions {
        accounts
            .entry(instruction.program_id)
            .or_default()
            .is_invoked = true;
        for account_meta in &instruction.accounts {
            let account = accounts.entry(account_meta.pubkey).or_default();
            account.is_signer |= account_meta.is_signer;
            account.is_writable |= account_meta.is_writable;
        }
    }
    if let Some(nonce) = nonce_pubkey(instructions) {
        accounts.entry(nonce).or_default().is_nonce = true;
    }
    let payer_account = accounts.entry(payer).or_default();
    payer_account.is_signer = true;
    payer_account.is_writable = true;
    accounts
}

fn nonce_pubkey(instructions: &[Instruction]) -> Option<Pubkey> {
    let instruction = instructions.first()?;
    if instruction.program_id != system_program::ID
        || instruction.data.get(..ADVANCE_NONCE_PREFIX.len())
            != Some(ADVANCE_NONCE_PREFIX.as_slice())
    {
        return None;
    }
    instruction.accounts.first().map(|account| account.pubkey)
}

fn validate_access(
    address: Pubkey,
    actual: LookupTableAccountAccess,
    compiler_is_writable: bool,
) -> Result<(), LookupTableManifestError> {
    let expected = LookupTableAccountAccess::from_writable(compiler_is_writable);
    if actual == expected {
        Ok(())
    } else {
        Err(LookupTableManifestError::AccessMismatch {
            address,
            expected,
            actual,
        })
    }
}

fn hash_input_prefix(domain: &[u8]) -> Vec<u8> {
    let mut output = Vec::with_capacity(domain.len() + 1);
    output.extend_from_slice(domain);
    output.push(0);
    output
}

fn append_static_requirements(output: &mut Vec<u8>, requirements: &[MustRemainStatic]) {
    append_len(output, requirements.len());
    for requirement in requirements {
        output.push(0);
        output.extend_from_slice(requirement.address.as_ref());
        output.push(requirement.access.hash_tag());
        append_len(output, requirement.reasons.len());
        output.extend(requirement.reasons.iter().map(|reason| reason.hash_tag()));
    }
}

fn append_shared_market_requirements(output: &mut Vec<u8>, requirements: &[SharedMarket]) {
    append_len(output, requirements.len());
    for requirement in requirements {
        output.push(1);
        output.extend_from_slice(requirement.address.as_ref());
        output.push(requirement.access.hash_tag());
        append_len(output, requirement.roles.len());
        output.extend(requirement.roles.iter().map(|role| role.hash_tag()));
    }
}

fn append_vault_requirements(output: &mut Vec<u8>, requirements: &[Vault]) {
    append_len(output, requirements.len());
    for requirement in requirements {
        output.push(2);
        output.extend_from_slice(requirement.address.as_ref());
        output.push(requirement.access.hash_tag());
        append_len(output, requirement.roles.len());
        output.extend(requirement.roles.iter().map(|role| role.hash_tag()));
    }
}

fn append_len(output: &mut Vec<u8>, len: usize) {
    output.extend_from_slice(&(len as u64).to_le_bytes());
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum LookupTableManifestError {
    ConflictingProvenance {
        address: Pubkey,
    },
    MissingProvenance {
        address: Pubkey,
    },
    OverlappingManifest {
        address: Pubkey,
    },
    IncompleteManifest {
        missing: Vec<Pubkey>,
        extra: Vec<Pubkey>,
    },
    StaticReasonMismatch {
        address: Pubkey,
    },
    StaticAccountInLookupManifest {
        address: Pubkey,
    },
    AccessMismatch {
        address: Pubkey,
        expected: LookupTableAccountAccess,
        actual: LookupTableAccountAccess,
    },
}

impl fmt::Display for LookupTableManifestError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::ConflictingProvenance { address } => write!(
                formatter,
                "account {address} has both shared-market and vault provenance"
            ),
            Self::MissingProvenance { address } => write!(
                formatter,
                "ALT-eligible account {address} is missing route-builder provenance"
            ),
            Self::OverlappingManifest { address } => {
                write!(formatter, "account {address} appears in multiple manifest classes")
            }
            Self::IncompleteManifest { missing, extra } => write!(
                formatter,
                "manifest does not match compiler account universe: missing={missing:?}, extra={extra:?}"
            ),
            Self::StaticReasonMismatch { address } => write!(
                formatter,
                "static reasons for account {address} do not match compiler requirements"
            ),
            Self::StaticAccountInLookupManifest { address } => write!(
                formatter,
                "compiler-static account {address} appears in an ALT-eligible manifest"
            ),
            Self::AccessMismatch {
                address,
                expected,
                actual,
            } => write!(
                formatter,
                "account {address} access mismatch: compiler={expected:?}, manifest={actual:?}"
            ),
        }
    }
}

impl std::error::Error for LookupTableManifestError {}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_sdk::{
        hash::Hash,
        instruction::AccountMeta,
        message::{v0, AddressLookupTableAccount},
    };

    fn instruction_fixture() -> (Pubkey, Vec<Instruction>, Pubkey, Pubkey, Pubkey, Pubkey) {
        let payer = Pubkey::new_unique();
        let nonce = Pubkey::new_unique();
        let nonce_authority = Pubkey::new_unique();
        let invoked_program = Pubkey::new_unique();
        let shared = Pubkey::new_unique();
        let vault = Pubkey::new_unique();
        let nonce_instruction = Instruction {
            program_id: system_program::ID,
            accounts: vec![
                AccountMeta::new(nonce, false),
                AccountMeta::new_readonly(nonce_authority, true),
            ],
            data: ADVANCE_NONCE_PREFIX.to_vec(),
        };
        let route_instruction = Instruction {
            program_id: invoked_program,
            accounts: vec![
                AccountMeta::new_readonly(shared, false),
                AccountMeta::new(vault, false),
                AccountMeta::new_readonly(invoked_program, false),
            ],
            data: vec![1],
        };
        (
            payer,
            vec![nonce_instruction, route_instruction],
            nonce,
            nonce_authority,
            shared,
            vault,
        )
    }

    #[test]
    fn manifest_matches_v0_compiler_static_and_lookup_eligible_sets() {
        let (payer, instructions, nonce, nonce_authority, shared, vault) = instruction_fixture();
        let mut provenance = LookupTableAccountProvenance::default();
        provenance.add_shared_market(shared, SharedMarketRole::Reserve);
        provenance.add_vault(vault, VaultRole::Obligation);

        let manifest = LookupTableManifest::from_instructions(payer, &instructions, &provenance)
            .expect("fixture provenance should be complete");

        let static_addresses = manifest
            .must_remain_static()
            .iter()
            .map(|requirement| requirement.address)
            .collect::<BTreeSet<_>>();
        assert!(static_addresses.contains(&payer));
        assert!(static_addresses.contains(&nonce));
        assert!(static_addresses.contains(&nonce_authority));
        assert!(static_addresses.contains(&system_program::ID));
        assert!(static_addresses.contains(&instructions[1].program_id));
        assert_eq!(manifest.lookup_eligible_addresses(), vec![shared, vault]);

        let table = AddressLookupTableAccount {
            key: Pubkey::new_unique(),
            addresses: manifest.lookup_eligible_addresses(),
        };
        let message = v0::Message::try_compile(&payer, &instructions, &[table], Hash::new_unique())
            .expect("manifest should compile through the production v0 compiler");
        assert_eq!(message.address_table_lookups.len(), 1);
        assert_eq!(message.address_table_lookups[0].writable_indexes, vec![1]);
        assert_eq!(message.address_table_lookups[0].readonly_indexes, vec![0]);
    }

    #[test]
    fn manifest_rejects_shared_and_vault_overlap() {
        let (payer, instructions, _, _, shared, vault) = instruction_fixture();
        let mut provenance = LookupTableAccountProvenance::default();
        provenance.add_shared_market(shared, SharedMarketRole::Reserve);
        provenance.add_shared_market(vault, SharedMarketRole::Reserve);
        provenance.add_vault(vault, VaultRole::Obligation);

        assert_eq!(
            LookupTableManifest::from_instructions(payer, &instructions, &provenance),
            Err(LookupTableManifestError::ConflictingProvenance { address: vault })
        );
    }

    #[test]
    fn manifest_rejects_incomplete_lookup_provenance() {
        let (payer, instructions, _, _, shared, vault) = instruction_fixture();
        let mut provenance = LookupTableAccountProvenance::default();
        provenance.add_shared_market(shared, SharedMarketRole::Reserve);

        assert_eq!(
            LookupTableManifest::from_instructions(payer, &instructions, &provenance),
            Err(LookupTableManifestError::MissingProvenance { address: vault })
        );
    }

    #[test]
    fn manifest_order_and_hash_inputs_are_deterministic() {
        let (payer, instructions, _, _, shared, vault) = instruction_fixture();
        let unused = Pubkey::new_unique();
        let mut first = LookupTableAccountProvenance::default();
        first.add_vault(vault, VaultRole::VaultTokenAccount);
        first.add_shared_market(shared, SharedMarketRole::Reserve);
        first.add_shared_market(shared, SharedMarketRole::LiquiditySupply);
        first.add_shared_market(unused, SharedMarketRole::Market);
        let mut second = LookupTableAccountProvenance::default();
        second.add_shared_market(unused, SharedMarketRole::Market);
        second.add_shared_market(shared, SharedMarketRole::LiquiditySupply);
        second.add_shared_market(shared, SharedMarketRole::Reserve);
        second.add_vault(vault, VaultRole::VaultTokenAccount);

        let first = LookupTableManifest::from_instructions(payer, &instructions, &first)
            .expect("first provenance order should compile");
        let second = LookupTableManifest::from_instructions(payer, &instructions, &second)
            .expect("second provenance order should compile");

        assert_eq!(first, second);
        assert_eq!(first.canonical_hash_input(), second.canonical_hash_input());
        assert_eq!(
            first.shared_market_hash_input(),
            second.shared_market_hash_input()
        );
        assert_eq!(first.vault_hash_input(), second.vault_hash_input());
    }

    #[test]
    fn semantic_route_requirements_own_roles_and_exact_compiler_classes() {
        let payer = Pubkey::new_unique();
        let program = Pubkey::new_unique();
        let settings = Pubkey::new_unique();
        let vault = Pubkey::new_unique();
        let policy = Pubkey::new_unique();
        let action = Pubkey::new_unique();
        let obligation = Pubkey::new_unique();
        let vault_token = Pubkey::new_unique();
        let metadata = Pubkey::new_unique();
        let farm_user = Pubkey::new_unique();
        let market = Pubkey::new_unique();
        let market_authority = Pubkey::new_unique();
        let reserve = Pubkey::new_unique();
        let liquidity_mint = Pubkey::new_unique();
        let liquidity_supply = Pubkey::new_unique();
        let collateral_mint = Pubkey::new_unique();
        let collateral_supply = Pubkey::new_unique();
        let oracle = Pubkey::new_unique();
        let scope_prices = Pubkey::new_unique();
        let reserve_farm = Pubkey::new_unique();
        let infrastructure = Pubkey::new_unique();

        let mut requirements = YieldRouteLookupTableRequirements::new(settings, vault);
        requirements.add_policy(policy);
        requirements.add_action_account(action);
        requirements.add_obligation(obligation);
        requirements.add_vault_token_account(vault_token);
        requirements.add_metadata(metadata);
        requirements.add_farm_user_state(farm_user);
        let mut reserve_accounts =
            KaminoReserveLookupTableAccounts::new(market, reserve, liquidity_mint);
        reserve_accounts.market_authorities = vec![market_authority];
        reserve_accounts.liquidity_supply = Some(liquidity_supply);
        reserve_accounts.collateral_mint = Some(collateral_mint);
        reserve_accounts.collateral_supply = Some(collateral_supply);
        reserve_accounts.oracles = vec![oracle];
        reserve_accounts.scope_prices = Some(scope_prices);
        reserve_accounts.reserve_farm_state = Some(reserve_farm);
        reserve_accounts.infrastructure = vec![infrastructure];
        requirements.add_kamino_reserve(reserve_accounts);

        let shared = [
            market,
            market_authority,
            reserve,
            liquidity_mint,
            liquidity_supply,
            collateral_mint,
            collateral_supply,
            oracle,
            scope_prices,
            reserve_farm,
            infrastructure,
        ];
        let vault_dependent = [
            settings,
            vault,
            policy,
            action,
            obligation,
            vault_token,
            metadata,
            farm_user,
        ];
        let instruction = Instruction {
            program_id: program,
            accounts: shared
                .into_iter()
                .map(|address| AccountMeta::new_readonly(address, false))
                .chain(
                    vault_dependent
                        .into_iter()
                        .map(|address| AccountMeta::new(address, false)),
                )
                .collect(),
            data: vec![1],
        };
        let manifest = requirements
            .manifest(payer, std::slice::from_ref(&instruction))
            .expect("semantic requirements should exactly classify the compiler universe");

        assert_eq!(
            manifest
                .shared_market()
                .iter()
                .map(|requirement| requirement.address)
                .collect::<BTreeSet<_>>(),
            shared.into_iter().collect()
        );
        assert_eq!(
            manifest
                .vault()
                .iter()
                .map(|requirement| requirement.address)
                .collect::<BTreeSet<_>>(),
            vault_dependent.into_iter().collect()
        );
        assert!(manifest
            .vault()
            .iter()
            .all(|requirement| requirement.access == LookupTableAccountAccess::Writable));
        assert!(requirements
            .provenance()
            .shared_market_roles(&oracle)
            .is_some_and(|roles| roles.contains(&SharedMarketRole::Oracle)));
        assert!(requirements
            .provenance()
            .vault_roles(&farm_user)
            .is_some_and(|roles| roles.contains(&VaultRole::FarmUserState)));
    }

    #[test]
    fn semantic_requirement_composition_rejects_cross_class_overlap() {
        let conflicting = Pubkey::new_unique();
        let mut vault_requirements = YieldRouteLookupTableRequirements::default();
        vault_requirements.add_vault_token_account(conflicting);
        let mut shared_requirements = YieldRouteLookupTableRequirements::default();
        shared_requirements.add_infrastructure(conflicting);

        assert_eq!(
            vault_requirements.merge(&shared_requirements),
            Err(LookupTableManifestError::ConflictingProvenance {
                address: conflicting,
            })
        );
    }
}
