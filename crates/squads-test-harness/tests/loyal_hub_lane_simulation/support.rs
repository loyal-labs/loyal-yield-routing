use loyal_actions::{
    create_swap_yield_route_action, LoyalActionContext, SwapLane, YieldRouteActionInstruction,
    YIELD_ROUTE_STANDALONE_ACTION_SEED,
};
use solana_sdk::{
    pubkey::Pubkey,
    signature::{Keypair, Signer},
};
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs, create_squads_smart_account_instruction,
    derive_squads_pool, derive_squads_vault, get_spl_token_amount,
    initialize_loyal_hub_config_instruction_with_rebalancer_and_lane_count,
    loyal_hub_lane_token_account, rebalance_loyal_hub_inventory_instruction,
    seed_loyal_hub_inventory_spl_accounts_for_lane, seed_spl_mint_if_missing,
    seed_spl_token_account, try_send_instructions, FundedSquadsTestContext, HubSwapExecution,
    LoyalHubRebalanceTransfer, MockProgram, RouteActionExt, LAMPORTS_PER_SOL, PYUSD_DECIMALS,
    PYUSD_MINT, USDC_DECIMALS, USDC_MINT,
};

pub(super) const WALLET_COUNT: usize = 30;
pub(super) const DEFAULT_LANE_COUNT: u8 = 4;
pub(super) const GROWTH_LANE_COUNT: u8 = 32;
const VAULT_INDEX: u8 = 0;
const MAX_FEE_BPS: u16 = 10;
const HUB_FEE_BPS: u64 = 10;
const WALLET_START_USDC: u64 = 4_000_000;
const WALLET_START_PYUSD: u64 = 4_000_000;
const LANE_START_USDC: u64 = 10_000_000;
const LANE_START_PYUSD: u64 = 10_000_000;
const SIM_SEED_START: u128 = 2;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum SwapDirection {
    UsdcToPyusd,
    PyusdToUsdc,
}

impl SwapDirection {
    fn input_mint(self) -> Pubkey {
        match self {
            Self::UsdcToPyusd => USDC_MINT,
            Self::PyusdToUsdc => PYUSD_MINT,
        }
    }

    fn output_mint(self) -> Pubkey {
        match self {
            Self::UsdcToPyusd => PYUSD_MINT,
            Self::PyusdToUsdc => USDC_MINT,
        }
    }
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(super) struct TokenPair {
    pub(super) usdc: u64,
    pub(super) pyusd: u64,
}

impl TokenPair {
    fn amount(self, mint: Pubkey) -> u64 {
        if mint == USDC_MINT {
            self.usdc
        } else if mint == PYUSD_MINT {
            self.pyusd
        } else {
            panic!("unexpected mint {mint}");
        }
    }

    fn add(&mut self, mint: Pubkey, amount: u64) {
        if mint == USDC_MINT {
            self.usdc += amount;
        } else if mint == PYUSD_MINT {
            self.pyusd += amount;
        } else {
            panic!("unexpected mint {mint}");
        }
    }

    fn sub(&mut self, mint: Pubkey, amount: u64) {
        if mint == USDC_MINT {
            self.usdc -= amount;
        } else if mint == PYUSD_MINT {
            self.pyusd -= amount;
        } else {
            panic!("unexpected mint {mint}");
        }
    }

    fn min_with(&mut self, other: TokenPair) {
        self.usdc = self.usdc.min(other.usdc);
        self.pyusd = self.pyusd.min(other.pyusd);
    }
}

pub(super) struct SimWallet {
    signer: Keypair,
    vault: Pubkey,
    vault_usdc: Pubkey,
    vault_pyusd: Pubkey,
    pub(super) swap_action: YieldRouteActionInstruction,
}

impl SimWallet {
    fn input_account(&self, direction: SwapDirection) -> Pubkey {
        match direction {
            SwapDirection::UsdcToPyusd => self.vault_usdc,
            SwapDirection::PyusdToUsdc => self.vault_pyusd,
        }
    }

    fn output_account(&self, direction: SwapDirection) -> Pubkey {
        match direction {
            SwapDirection::UsdcToPyusd => self.vault_pyusd,
            SwapDirection::PyusdToUsdc => self.vault_usdc,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) struct PlannedSwap {
    pub(super) wallet_index: usize,
    pub(super) lane_id: u8,
    pub(super) direction: SwapDirection,
    pub(super) amount_in: u64,
}

impl PlannedSwap {
    fn amount_out(self) -> u64 {
        self.amount_in - (self.amount_in * HUB_FEE_BPS / 10_000)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct SwapWave {
    swaps: Vec<PlannedSwap>,
}

impl SwapWave {
    pub(super) fn new(swaps: Vec<PlannedSwap>) -> Self {
        Self { swaps }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) struct PlannedRebalance {
    pub(super) mint: Pubkey,
    pub(super) from_lane_id: u8,
    pub(super) to_lane_id: u8,
    pub(super) amount: u64,
}

impl PlannedRebalance {
    fn transfer(self) -> LoyalHubRebalanceTransfer {
        LoyalHubRebalanceTransfer {
            from_lane_id: self.from_lane_id,
            to_lane_id: self.to_lane_id,
            amount: self.amount,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) enum SimulationEvent {
    SwapAccepted {
        swap: PlannedSwap,
        amount_out: u64,
    },
    SwapRejected {
        swap: PlannedSwap,
        error: String,
    },
    RebalanceAccepted {
        transfer: PlannedRebalance,
    },
    RebalanceBlocked {
        active_lanes: Vec<u8>,
        transfers: Vec<PlannedRebalance>,
        reason: String,
    },
}

#[derive(Clone, Debug)]
struct InitialBalances {
    lanes: Vec<TokenPair>,
    wallets: Vec<TokenPair>,
}

impl InitialBalances {
    fn new(lane_count: u8, wallet_count: usize) -> Self {
        Self {
            lanes: vec![
                TokenPair {
                    usdc: LANE_START_USDC,
                    pyusd: LANE_START_PYUSD,
                };
                usize::from(lane_count)
            ],
            wallets: vec![
                TokenPair {
                    usdc: WALLET_START_USDC,
                    pyusd: WALLET_START_PYUSD,
                };
                wallet_count
            ],
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct BalanceLedger {
    lanes: Vec<TokenPair>,
    wallets: Vec<TokenPair>,
}

impl BalanceLedger {
    fn apply_swap(&mut self, swap: PlannedSwap, amount_out: u64) {
        let input_mint = swap.direction.input_mint();
        let output_mint = swap.direction.output_mint();

        self.lanes[usize::from(swap.lane_id)].add(input_mint, swap.amount_in);
        self.lanes[usize::from(swap.lane_id)].sub(output_mint, amount_out);
        self.wallets[swap.wallet_index].sub(input_mint, swap.amount_in);
        self.wallets[swap.wallet_index].add(output_mint, amount_out);
    }

    fn apply_rebalance(&mut self, transfer: PlannedRebalance) {
        self.lanes[usize::from(transfer.from_lane_id)].sub(transfer.mint, transfer.amount);
        self.lanes[usize::from(transfer.to_lane_id)].add(transfer.mint, transfer.amount);
    }

    pub(super) fn lane_amount(&self, lane_id: u8, mint: Pubkey) -> u64 {
        self.lanes[usize::from(lane_id)].amount(mint)
    }

    pub(super) fn wallet_amount(&self, wallet_index: usize, mint: Pubkey) -> u64 {
        self.wallets[wallet_index].amount(mint)
    }

    fn assert_matches_chain(
        &self,
        context: &FundedSquadsTestContext,
        wallets: &[SimWallet],
        lane_count: u8,
    ) {
        for lane_id in 0..lane_count {
            let expected = self.lanes[usize::from(lane_id)];
            assert_eq!(
                get_spl_token_amount(
                    &context.svm,
                    loyal_hub_lane_token_account(USDC_MINT, lane_id)
                ),
                expected.usdc,
                "lane {lane_id} USDC"
            );
            assert_eq!(
                get_spl_token_amount(
                    &context.svm,
                    loyal_hub_lane_token_account(PYUSD_MINT, lane_id)
                ),
                expected.pyusd,
                "lane {lane_id} PYUSD"
            );
        }

        for (index, wallet) in wallets.iter().enumerate() {
            let expected = self.wallets[index];
            assert_eq!(
                get_spl_token_amount(&context.svm, wallet.vault_usdc),
                expected.usdc,
                "wallet {index} USDC"
            );
            assert_eq!(
                get_spl_token_amount(&context.svm, wallet.vault_pyusd),
                expected.pyusd,
                "wallet {index} PYUSD"
            );
        }
    }

    fn assert_total_conservation(&self, initial: &InitialBalances) {
        let lane_totals = self
            .lanes
            .iter()
            .fold(TokenPair::default(), |mut totals, lane| {
                totals.usdc += lane.usdc;
                totals.pyusd += lane.pyusd;
                totals
            });
        let wallet_totals = self
            .wallets
            .iter()
            .fold(TokenPair::default(), |mut totals, wallet| {
                totals.usdc += wallet.usdc;
                totals.pyusd += wallet.pyusd;
                totals
            });
        let initial_lane_totals =
            initial
                .lanes
                .iter()
                .fold(TokenPair::default(), |mut totals, lane| {
                    totals.usdc += lane.usdc;
                    totals.pyusd += lane.pyusd;
                    totals
                });
        let initial_wallet_totals =
            initial
                .wallets
                .iter()
                .fold(TokenPair::default(), |mut totals, wallet| {
                    totals.usdc += wallet.usdc;
                    totals.pyusd += wallet.pyusd;
                    totals
                });

        assert_eq!(
            lane_totals.usdc + wallet_totals.usdc,
            initial_lane_totals.usdc + initial_wallet_totals.usdc,
            "total USDC conservation"
        );
        assert_eq!(
            lane_totals.pyusd + wallet_totals.pyusd,
            initial_lane_totals.pyusd + initial_wallet_totals.pyusd,
            "total PYUSD conservation"
        );
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) struct LaneMetric {
    pub(super) inflow: TokenPair,
    pub(super) outflow: TokenPair,
    pub(super) minimum_inventory: TokenPair,
    pub(super) failed_swap_count: u64,
    pub(super) rebalance_volume: TokenPair,
}

impl LaneMetric {
    fn new(initial_inventory: TokenPair) -> Self {
        Self {
            inflow: TokenPair::default(),
            outflow: TokenPair::default(),
            minimum_inventory: initial_inventory,
            failed_swap_count: 0,
            rebalance_volume: TokenPair::default(),
        }
    }
}

struct DerivedState {
    ledger: BalanceLedger,
    metrics: Vec<LaneMetric>,
}

impl DerivedState {
    fn from_events(initial: &InitialBalances, events: &[SimulationEvent]) -> Self {
        let mut ledger = BalanceLedger {
            lanes: initial.lanes.clone(),
            wallets: initial.wallets.clone(),
        };
        let mut metrics = initial
            .lanes
            .iter()
            .copied()
            .map(LaneMetric::new)
            .collect::<Vec<_>>();

        for event in events {
            match event {
                SimulationEvent::SwapAccepted { swap, amount_out } => {
                    ledger.apply_swap(*swap, *amount_out);

                    let lane_metric = &mut metrics[usize::from(swap.lane_id)];
                    lane_metric
                        .inflow
                        .add(swap.direction.input_mint(), swap.amount_in);
                    lane_metric
                        .outflow
                        .add(swap.direction.output_mint(), *amount_out);
                    lane_metric
                        .minimum_inventory
                        .min_with(ledger.lanes[usize::from(swap.lane_id)]);
                }
                SimulationEvent::SwapRejected { swap, .. } => {
                    metrics[usize::from(swap.lane_id)].failed_swap_count += 1;
                }
                SimulationEvent::RebalanceAccepted { transfer } => {
                    ledger.apply_rebalance(*transfer);

                    for lane_id in [transfer.from_lane_id, transfer.to_lane_id] {
                        let lane_metric = &mut metrics[usize::from(lane_id)];
                        lane_metric
                            .rebalance_volume
                            .add(transfer.mint, transfer.amount);
                        lane_metric
                            .minimum_inventory
                            .min_with(ledger.lanes[usize::from(lane_id)]);
                    }
                }
                SimulationEvent::RebalanceBlocked { .. } => {}
            }
        }

        Self { ledger, metrics }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct ActiveWave {
    active_lanes: Vec<u8>,
}

impl ActiveWave {
    pub(super) fn from_wave(wave: &SwapWave) -> Self {
        Self {
            active_lanes: LaneScheduler::active_lanes(wave),
        }
    }

    pub(super) fn active_lanes(&self) -> &[u8] {
        &self.active_lanes
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) struct MaintenanceWindow;

pub(super) struct LaneScheduler;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) struct LaneCandidate {
    pub(super) lane_id: u8,
    pub(super) output_inventory: u64,
    pub(super) in_flight_count: u16,
}

impl LaneScheduler {
    fn active_lanes(wave: &SwapWave) -> Vec<u8> {
        let mut lanes = Vec::new();
        for swap in &wave.swaps {
            if !lanes.contains(&swap.lane_id) {
                lanes.push(swap.lane_id);
            }
        }
        lanes
    }

    pub(super) fn ensure_rebalance_avoids_active_lanes(
        active_lanes: &[u8],
        transfers: &[PlannedRebalance],
    ) -> Result<(), String> {
        for transfer in transfers {
            if active_lanes.contains(&transfer.from_lane_id)
                || active_lanes.contains(&transfer.to_lane_id)
            {
                return Err(format!(
                    "rebalance touches active lane {} -> {}",
                    transfer.from_lane_id, transfer.to_lane_id
                ));
            }
        }
        Ok(())
    }

    pub(super) fn choose_swap_lane(
        candidates: &[LaneCandidate],
        required_output_amount: u64,
    ) -> Option<u8> {
        candidates
            .iter()
            .filter(|candidate| candidate.output_inventory >= required_output_amount)
            .min_by_key(|candidate| (candidate.in_flight_count, candidate.lane_id))
            .map(|candidate| candidate.lane_id)
    }
}

pub(super) struct InventoryPlanner {
    pub(super) threshold: u64,
    pub(super) target: u64,
    pub(super) max_transfer_amount: u64,
}

impl InventoryPlanner {
    pub(super) fn plan_refill(
        &self,
        ledger: &BalanceLedger,
        target_lane_id: u8,
        mint: Pubkey,
    ) -> Vec<PlannedRebalance> {
        let current = ledger.lane_amount(target_lane_id, mint);
        if current >= self.threshold {
            return Vec::new();
        }

        let deficit = self.target.saturating_sub(current);
        let Some((source_lane_id, source_surplus)) =
            self.highest_surplus_lane(ledger, target_lane_id, mint)
        else {
            return Vec::new();
        };

        let amount = deficit.min(source_surplus).min(self.max_transfer_amount);
        if amount == 0 {
            Vec::new()
        } else {
            vec![PlannedRebalance {
                mint,
                from_lane_id: source_lane_id,
                to_lane_id: target_lane_id,
                amount,
            }]
        }
    }

    fn highest_surplus_lane(
        &self,
        ledger: &BalanceLedger,
        target_lane_id: u8,
        mint: Pubkey,
    ) -> Option<(u8, u64)> {
        ledger
            .lanes
            .iter()
            .enumerate()
            .fold(None, |best, (lane_index, lane)| {
                let lane_id = u8::try_from(lane_index).expect("lane index fits in u8");
                if lane_id == target_lane_id {
                    return best;
                }
                let surplus = lane.amount(mint).saturating_sub(self.target);
                if surplus == 0 {
                    return best;
                }

                match best {
                    Some((_, best_surplus)) if best_surplus >= surplus => best,
                    _ => Some((lane_id, surplus)),
                }
            })
    }
}

pub(super) struct HubLaneSimulation {
    context: FundedSquadsTestContext,
    hub_authorizer: Keypair,
    inventory_rebalancer: Keypair,
    pub(super) wallets: Vec<SimWallet>,
    lane_count: u8,
    initial_balances: InitialBalances,
    events: Vec<SimulationEvent>,
}

impl HubLaneSimulation {
    pub(super) fn setup(lane_count: u8, wallet_count: usize) -> Option<Self> {
        let Some(mut context) =
            create_funded_squads_test_context_with_mock_programs(&[MockProgram::LoyalHubSwap])
                .expect("create funded Squads test context")
        else {
            eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
            return None;
        };

        let hub_authorizer = Keypair::new();
        let inventory_rebalancer = Keypair::new();
        context
            .svm
            .airdrop(&hub_authorizer.pubkey(), LAMPORTS_PER_SOL / 10)
            .expect("airdrop hub authorizer");
        context
            .svm
            .airdrop(&inventory_rebalancer.pubkey(), LAMPORTS_PER_SOL / 10)
            .expect("airdrop inventory rebalancer");

        seed_spl_mint_if_missing(&mut context.svm, USDC_MINT, None, USDC_DECIMALS, 0);
        seed_spl_mint_if_missing(&mut context.svm, PYUSD_MINT, None, PYUSD_DECIMALS, 0);
        for lane_id in 0..lane_count {
            seed_loyal_hub_inventory_spl_accounts_for_lane(
                &mut context.svm,
                &[USDC_MINT, PYUSD_MINT],
                0,
                lane_id,
            );
        }

        let init_ix = initialize_loyal_hub_config_instruction_with_rebalancer_and_lane_count(
            context.wallet_pubkey(),
            context.wallet_pubkey(),
            hub_authorizer.pubkey(),
            inventory_rebalancer.pubkey(),
            50,
            false,
            lane_count,
            &[USDC_MINT, PYUSD_MINT],
        );
        try_send_instructions(&mut context.svm, &[init_ix], &context.wallet, &[])
            .expect("initialize Loyal Hub config");

        for lane_id in 0..lane_count {
            seed_loyal_hub_inventory_spl_accounts_for_lane(
                &mut context.svm,
                &[USDC_MINT],
                LANE_START_USDC,
                lane_id,
            );
            seed_loyal_hub_inventory_spl_accounts_for_lane(
                &mut context.svm,
                &[PYUSD_MINT],
                LANE_START_PYUSD,
                lane_id,
            );
        }

        let mut wallets = Vec::with_capacity(wallet_count);
        for index in 0..wallet_count {
            wallets.push(create_sim_wallet(
                &mut context,
                SIM_SEED_START + index as u128,
                hub_authorizer.pubkey(),
            ));
        }

        Some(Self {
            context,
            hub_authorizer,
            inventory_rebalancer,
            wallets,
            lane_count,
            initial_balances: InitialBalances::new(lane_count, wallet_count),
            events: Vec::new(),
        })
    }

    pub(super) fn execute_wave(&mut self, wave: &SwapWave) -> ActiveWave {
        let active_wave = ActiveWave::from_wave(wave);
        assert!(
            !active_wave.active_lanes.is_empty(),
            "simulation wave must exercise at least one lane"
        );

        for swap in &wave.swaps {
            self.execute_swap(*swap)
                .unwrap_or_else(|error| panic!("swap {swap:?} failed: {error}"));
        }
        active_wave
    }

    pub(super) fn settle_wave(&self, _wave: ActiveWave) -> MaintenanceWindow {
        MaintenanceWindow
    }

    pub(super) fn execute_swap(&mut self, swap: PlannedSwap) -> Result<(), String> {
        let wallet = &self.wallets[swap.wallet_index];
        let amount_out = swap.amount_out();
        let ix = wallet
            .swap_action
            .hub()
            .expect("swap action has Loyal Hub lane")
            .build(HubSwapExecution {
                signer: wallet.signer.pubkey(),
                vault_index: VAULT_INDEX,
                vault: wallet.vault,
                vault_input: wallet.input_account(swap.direction),
                vault_output: wallet.output_account(swap.direction),
                input_mint: swap.direction.input_mint(),
                output_mint: swap.direction.output_mint(),
                hub_authorizer: self.hub_authorizer.pubkey(),
                amount_in: swap.amount_in,
                amount_out,
                min_out: amount_out,
                max_fee_bps: MAX_FEE_BPS,
                lane_id: swap.lane_id,
            });

        let result = try_send_instructions(
            &mut self.context.svm,
            &[ix],
            &wallet.signer,
            &[&self.hub_authorizer],
        );
        match result {
            Ok(()) => {
                self.events
                    .push(SimulationEvent::SwapAccepted { swap, amount_out });
                self.assert_invariants();
                Ok(())
            }
            Err(error) => {
                self.events.push(SimulationEvent::SwapRejected {
                    swap,
                    error: error.clone(),
                });
                self.assert_all_balances();
                Err(error)
            }
        }
    }

    pub(super) fn rebalance_during_active_wave(
        &mut self,
        active_wave: &ActiveWave,
        transfers: &[PlannedRebalance],
    ) -> Result<(), String> {
        if let Err(reason) = LaneScheduler::ensure_rebalance_avoids_active_lanes(
            active_wave.active_lanes(),
            transfers,
        ) {
            self.events.push(SimulationEvent::RebalanceBlocked {
                active_lanes: active_wave.active_lanes.clone(),
                transfers: transfers.to_vec(),
                reason: reason.clone(),
            });
            self.assert_all_balances();
            return Err(reason);
        }

        self.submit_rebalances(transfers)
    }

    pub(super) fn rebalance_during_maintenance(
        &mut self,
        _maintenance: &MaintenanceWindow,
        transfers: &[PlannedRebalance],
    ) -> Result<(), String> {
        self.submit_rebalances(transfers)
    }

    pub(super) fn ledger(&self) -> BalanceLedger {
        self.derived_state().ledger
    }

    pub(super) fn events(&self) -> &[SimulationEvent] {
        &self.events
    }

    pub(super) fn assert_all_balances(&self) {
        self.ledger()
            .assert_matches_chain(&self.context, &self.wallets, self.lane_count);
    }

    pub(super) fn assert_total_conservation(&self) {
        self.ledger()
            .assert_total_conservation(&self.initial_balances);
    }

    pub(super) fn metrics(&self, lane_id: u8) -> LaneMetric {
        self.derived_state().metrics[usize::from(lane_id)]
    }

    fn submit_rebalances(&mut self, transfers: &[PlannedRebalance]) -> Result<(), String> {
        let mut transfer_groups: Vec<(Pubkey, Vec<LoyalHubRebalanceTransfer>)> = Vec::new();
        for transfer in transfers {
            let rebalance_transfer = transfer.transfer();
            match transfer_groups
                .iter_mut()
                .find(|(mint, _)| *mint == transfer.mint)
            {
                Some((_, grouped_transfers)) => grouped_transfers.push(rebalance_transfer),
                None => transfer_groups.push((transfer.mint, vec![rebalance_transfer])),
            }
        }

        let instructions = transfer_groups
            .iter()
            .map(|(mint, grouped_transfers)| {
                rebalance_loyal_hub_inventory_instruction(
                    self.inventory_rebalancer.pubkey(),
                    *mint,
                    grouped_transfers,
                )
            })
            .collect::<Vec<_>>();
        try_send_instructions(
            &mut self.context.svm,
            &instructions,
            &self.context.wallet,
            &[&self.inventory_rebalancer],
        )?;

        for transfer in transfers {
            self.events.push(SimulationEvent::RebalanceAccepted {
                transfer: *transfer,
            });
        }
        self.assert_invariants();
        Ok(())
    }

    fn derived_state(&self) -> DerivedState {
        DerivedState::from_events(&self.initial_balances, &self.events)
    }

    fn assert_invariants(&self) {
        self.assert_all_balances();
        self.assert_total_conservation();
    }
}

fn create_sim_wallet(
    context: &mut FundedSquadsTestContext,
    seed: u128,
    hub_authorizer: Pubkey,
) -> SimWallet {
    let signer = Keypair::new();
    context
        .svm
        .airdrop(&signer.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop simulated wallet signer");

    let pool = derive_squads_pool(seed);
    let create_ix =
        create_squads_smart_account_instruction(context.wallet_pubkey(), signer.pubkey(), seed);
    try_send_instructions(&mut context.svm, &[create_ix], &context.wallet, &[])
        .expect("create simulated Squads smart account");

    let (vault, _) = derive_squads_vault(&pool.settings, VAULT_INDEX);
    context
        .svm
        .airdrop(&vault, LAMPORTS_PER_SOL / 10)
        .expect("airdrop simulated Squads vault");

    let vault_usdc = Keypair::new().pubkey();
    let vault_pyusd = Keypair::new().pubkey();
    seed_spl_token_account(
        &mut context.svm,
        vault_usdc,
        USDC_MINT,
        vault,
        WALLET_START_USDC,
    );
    seed_spl_token_account(
        &mut context.svm,
        vault_pyusd,
        PYUSD_MINT,
        vault,
        WALLET_START_PYUSD,
    );

    let swap_action = create_swap_yield_route_action(
        LoyalActionContext {
            settings: pool.settings,
            authority: signer.pubkey(),
            delegated_signer: signer.pubkey(),
            account_index: VAULT_INDEX,
            vault,
        },
        vec![USDC_MINT, PYUSD_MINT],
        vec![SwapLane::LoyalHub {
            hub_authorizer,
            max_fee_bps: 50,
        }],
        YIELD_ROUTE_STANDALONE_ACTION_SEED,
    )
    .expect("build simulated Loyal Hub swap action");
    try_send_instructions(
        &mut context.svm,
        &[swap_action.instruction.clone()],
        &signer,
        &[],
    )
    .expect("create simulated Loyal Hub action");

    SimWallet {
        signer,
        vault,
        vault_usdc,
        vault_pyusd,
        swap_action,
    }
}
