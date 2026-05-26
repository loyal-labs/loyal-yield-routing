//! Test-harness adapters for the `loyal-actions` SDK.

use loyal_actions::{LoyalActionContext, YieldRouteUniverse};
use solana_sdk::pubkey::Pubkey;

use crate::{FundedSquadsTestContext, MockKaminoReserveTokenAccounts};

pub fn loyal_action_context(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
) -> LoyalActionContext {
    LoyalActionContext {
        settings: context.pool.settings,
        authority: context.wallet_pubkey(),
        delegated_signer,
        account_index: context.vault_index,
        vault: context.vault,
    }
}

pub fn yield_route_universe_from_mock_reserves(
    stable_mints: Vec<Pubkey>,
    kamino_reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> YieldRouteUniverse {
    YieldRouteUniverse::new(
        stable_mints,
        kamino_reserves
            .iter()
            .map(|reserve| reserve.market)
            .collect(),
        kamino_reserves
            .iter()
            .map(|reserve| reserve.liquidity_mint)
            .collect(),
    )
}
