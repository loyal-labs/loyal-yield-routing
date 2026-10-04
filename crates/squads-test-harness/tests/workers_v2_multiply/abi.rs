//! ABI vectors emitted by the pinned official KLend SDK. These are instruction
//! construction proofs, not claims of KLend program execution or account state.
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use klend_interface::instructions::*;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

fn key(n: u8) -> Pubkey {
    Pubkey::new_from_array([n; 32])
}
fn encode(ix: &Instruction) -> Value {
    json!({"programId":ix.program_id.to_string(),"dataBase64":BASE64.encode(&ix.data),"accounts":ix.accounts.iter().map(|a|json!({"address":a.pubkey.to_string(),"signer":a.is_signer,"writable":a.is_writable})).collect::<Vec<_>>()})
}

#[test]
fn official_klend_complete_recipe_vectors() {
    let vault = key(60);
    let market = key(61);
    let authority = key(62);
    let oracle = key(63);
    let obligation = key(64);
    let collateral_reserve = key(65);
    let collateral_mint = key(66);
    let collateral_custody = key(67);
    let collateral_supply = key(68);
    let collateral_receipt = key(69);
    let collateral_mint_supply = key(70);
    let debt_reserve = key(71);
    let debt_mint = key(72);
    let debt_custody = key(73);
    let debt_supply = key(74);
    let debt_fee = key(75);
    let amount = 424_242;
    let mut cases = Vec::new();
    for token in [spl_token::id(), spl_token_2022::id()] {
        for farms in [false, true] {
            let collateral_farm_user = farms.then_some(key(76));
            let collateral_farm_state = farms.then_some(key(77));
            let debt_farm_user = farms.then_some(key(78));
            let debt_farm_state = farms.then_some(key(79));
            let input = json!({"Vault":vault.to_string(),"Amount":amount,"Config":{"Market":market.to_string(),"MarketAuthority":authority.to_string(),"Oracle":oracle.to_string(),"Obligation":obligation.to_string(),"CollateralReserve":collateral_reserve.to_string(),"CollateralMint":collateral_mint.to_string(),"CollateralCustody":collateral_custody.to_string(),"CollateralLiquiditySupply":collateral_supply.to_string(),"CollateralReceiptMint":collateral_receipt.to_string(),"CollateralMintSupply":collateral_mint_supply.to_string(),"CollateralFarmUser":collateral_farm_user.map(|v|v.to_string()),"CollateralFarmState":collateral_farm_state.map(|v|v.to_string()),"DebtReserve":debt_reserve.to_string(),"DebtMint":debt_mint.to_string(),"DebtCustody":debt_custody.to_string(),"DebtLiquiditySupply":debt_supply.to_string(),"DebtFeeVault":debt_fee.to_string(),"DebtTokenProgram":token.to_string(),"DebtFarmUser":debt_farm_user.map(|v|v.to_string()),"DebtFarmState":debt_farm_state.map(|v|v.to_string())}});
            let refresh = |reserve| {
                refresh_reserve(RefreshReserveAccounts {
                    reserve,
                    lending_market: market,
                    pyth_oracle: None,
                    switchboard_price_oracle: None,
                    switchboard_twap_oracle: None,
                    scope_prices: Some(oracle),
                })
            };
            let refresh_obligation_ix = |collateral, debt| {
                let mut remaining = Vec::new();
                if collateral {
                    remaining.push(AccountMeta::new(collateral_reserve, false));
                }
                if debt {
                    remaining.push(AccountMeta::new(debt_reserve, false));
                }
                refresh_obligation(
                    RefreshObligationAccounts {
                        lending_market: market,
                        obligation,
                    },
                    remaining,
                )
            };
            let deposit = deposit_reserve_liquidity_and_obligation_collateral_v2(
                DepositReserveLiquidityAndObligationCollateralV2Accounts {
                    owner: vault,
                    obligation,
                    lending_market: market,
                    lending_market_authority: authority,
                    reserve: collateral_reserve,
                    reserve_liquidity_mint: collateral_mint,
                    reserve_liquidity_supply: collateral_supply,
                    reserve_collateral_mint: collateral_receipt,
                    reserve_destination_deposit_collateral: collateral_mint_supply,
                    user_source_liquidity: collateral_custody,
                    placeholder_user_destination_collateral: None,
                    liquidity_token_program: spl_token::id(),
                    obligation_farm_user_state: collateral_farm_user,
                    reserve_farm_state: collateral_farm_state,
                },
                amount,
            );
            let borrow = borrow_obligation_liquidity_v2(
                BorrowObligationLiquidityV2Accounts {
                    owner: vault,
                    obligation,
                    lending_market: market,
                    lending_market_authority: authority,
                    borrow_reserve: debt_reserve,
                    borrow_reserve_liquidity_mint: debt_mint,
                    reserve_source_liquidity: debt_supply,
                    borrow_reserve_liquidity_fee_receiver: debt_fee,
                    user_destination_liquidity: debt_custody,
                    referrer_token_state: None,
                    token_program: token,
                    obligation_farm_user_state: debt_farm_user,
                    reserve_farm_state: debt_farm_state,
                },
                amount,
                Vec::new(),
            );
            let withdraw = withdraw_obligation_collateral_and_redeem_reserve_collateral_v2(
                WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts {
                    owner: vault,
                    obligation,
                    lending_market: market,
                    lending_market_authority: authority,
                    withdraw_reserve: collateral_reserve,
                    reserve_liquidity_mint: collateral_mint,
                    reserve_source_collateral: collateral_mint_supply,
                    reserve_collateral_mint: collateral_receipt,
                    reserve_liquidity_supply: collateral_supply,
                    user_destination_liquidity: collateral_custody,
                    placeholder_user_destination_collateral: None,
                    liquidity_token_program: spl_token::id(),
                    obligation_farm_user_state: collateral_farm_user,
                    reserve_farm_state: collateral_farm_state,
                },
                amount,
            );
            let repay = repay_obligation_liquidity_v2(
                RepayObligationLiquidityV2Accounts {
                    owner: vault,
                    obligation,
                    lending_market: market,
                    repay_reserve: debt_reserve,
                    reserve_liquidity_mint: debt_mint,
                    reserve_destination_liquidity: debt_supply,
                    user_source_liquidity: debt_custody,
                    token_program: token,
                    obligation_farm_user_state: debt_farm_user,
                    reserve_farm_state: debt_farm_state,
                    lending_market_authority: authority,
                },
                amount,
                Vec::new(),
            );
            for collateral in [false, true] {
                for debt in [false, true] {
                    for (action, terminal) in
                        [("deposit", deposit.clone()), ("borrow", borrow.clone())]
                    {
                        let recipe = vec![
                            refresh(collateral_reserve),
                            refresh(debt_reserve),
                            refresh_obligation_ix(collateral, debt),
                            terminal,
                        ];
                        cases.push(json!({"input":input,"action":action,"includeCollateral":collateral,"includeDebt":debt,"instructions":recipe.iter().map(encode).collect::<Vec<_>>() }));
                    }
                }
            }
            for debt in [false, true] {
                let mut recipe = vec![refresh(collateral_reserve)];
                if debt {
                    recipe.push(refresh(debt_reserve));
                }
                recipe.push(refresh_obligation_ix(true, debt));
                recipe.push(withdraw.clone());
                cases.push(json!({"input":input,"action":"withdraw","includeCollateral":true,"includeDebt":debt,"instructions":recipe.iter().map(encode).collect::<Vec<_>>() }));
            }
            let recipe = vec![
                refresh(collateral_reserve),
                refresh(debt_reserve),
                refresh_obligation_ix(true, true),
                repay,
            ];
            cases.push(json!({"input":input,"action":"repay","includeCollateral":true,"includeDebt":true,"instructions":recipe.iter().map(encode).collect::<Vec<_>>() }));
        }
    }
    assert_eq!(cases.len(), 44);
    if let Some(path) = std::env::var_os("WORKERS_V2_MULTIPLY_ABI_FIXTURE_OUTPUT") {
        let sdk_revision = "23b9f2b54530784ef1d9d0e08c5256e1eafe4a04";
        let source = include_bytes!("abi.rs");
        let fixture = json!({"provenance":{"producer":"official klend-interface SDK at locked Cargo revision","sdkRevision":sdk_revision,"cargoLockSha256":format!("{:x}",Sha256::digest(include_bytes!("../../../../Cargo.lock"))),"generatorSha256":format!("{:x}",Sha256::digest(source)),"proof":"full instruction recipes and account flags; no KLend program execution"},"cases":cases});
        std::fs::write(path, serde_json::to_vec_pretty(&fixture).unwrap()).unwrap();
    }
}
