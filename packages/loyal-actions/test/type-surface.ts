import { PublicKey, TransactionInstruction } from "@solana/web3.js";
import {
  LOYAL_CLUSTER_CONFIGS,
  LoyalCluster,
  RiskBasket,
  STABLECOIN_MINTS,
  Stablecoin,
  TREASURY_JUPITER_SWAP_ACTION_SEED,
  TREASURY_REBALANCE_ACTION_SEED,
  TREASURY_TOP_UP_ACTION_SEED,
  SwapLane,
  compileSquadsTransactionInstructions,
  createLoyalActionsSdk,
  createSquadsProgramInteractionExecutionInstruction,
  createSquadsSmartAccountInstruction,
  createSquadsSyncTransactionInstruction,
  deriveSquadsSettings,
  deriveSquadsVault,
} from "../src/index.js";

const sdk = createLoyalActionsSdk({ cluster: LoyalCluster.MainnetBeta });
const key = new PublicKey("11111111111111111111111111111112");

const policy = sdk.initYieldRoutePolicy({
  risk: RiskBasket.Safe,
  swapLanes: [SwapLane.Jupiter] as const,
  squads: {
    settings: key,
    authority: key,
    delegatedSigner: key,
    accountIndex: 0,
    vault: key,
  },
});

const jupiterIndexes = policy.routes.jupiter.instructionConstraintIndexes;
void jupiterIndexes;

const treasuryPolicy = sdk.initTreasuryLoyalHubRebalancePolicy({
  laneId: 0,
  inputMint: STABLECOIN_MINTS[Stablecoin.USDC],
  outputMint: STABLECOIN_MINTS[Stablecoin.PYUSD],
  inputTokenProgram: LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta].tokenProgramId,
  outputTokenProgram: LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta].token2022ProgramId,
  outputMintDecimals: 6,
  maxWithdrawAmount: 500000n,
  maxTopUpAmount: 495000n,
  maxSlippageBps: 50,
  squads: {
    settings: key,
    authority: key,
    delegatedSigner: key,
    accountIndex: 0,
    vault: key,
    policySeed: TREASURY_REBALANCE_ACTION_SEED,
    jupiterPolicySeed: TREASURY_JUPITER_SWAP_ACTION_SEED,
    topUpPolicySeed: TREASURY_TOP_UP_ACTION_SEED,
  },
});
const treasuryWithdrawIndexes = treasuryPolicy.policies.withdraw.route.instructionConstraintIndexes;
const treasuryJupiterIndexes = treasuryPolicy.policies.jupiter.route.instructionConstraintIndexes;
const treasuryTopUpIndexes = treasuryPolicy.policies.topUp.route.instructionConstraintIndexes;
void treasuryWithdrawIndexes;
void treasuryJupiterIndexes;
void treasuryTopUpIndexes;
void treasuryPolicy.policies.withdraw.constraints;
void treasuryPolicy.policies.jupiter.constraints;
void treasuryPolicy.policies.topUp.constraints;

sdk.initTreasuryLoyalHubRebalancePolicy({
  laneId: 0,
  inputMint: STABLECOIN_MINTS[Stablecoin.USDC],
  outputMint: STABLECOIN_MINTS[Stablecoin.PYUSD],
  inputTokenProgram: LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta].tokenProgramId,
  outputTokenProgram: LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta].token2022ProgramId,
  outputMintDecimals: 6,
  // @ts-expect-error Amount caps must be bigint raw amounts.
  maxWithdrawAmount: 500000,
  maxTopUpAmount: 495000n,
  maxSlippageBps: 50,
  squads: {
    settings: key,
    authority: key,
    delegatedSigner: key,
    accountIndex: 0,
    vault: key,
  },
});

// @ts-expect-error Loyal route metadata is absent when the Loyal lane is not enabled.
const loyalIndexes = policy.routes.loyal.instructionConstraintIndexes;
void loyalIndexes;

sdk.initYieldRoutePolicy({
  risk: RiskBasket.Safe,
  swapLanes: [SwapLane.Jupiter] as const,
  squads: {
    settings: key,
    authority: key,
    delegatedSigner: key,
    accountIndex: 0,
    vault: key,
  },
  // @ts-expect-error Stablecoin exposure is fixed by the SDK in v1.
  stablecoins: [],
});

sdk.initYieldRoutePolicy({
  risk: RiskBasket.Safe,
  swapLanes: [SwapLane.Jupiter] as const,
  squads: {
    settings: key,
    authority: key,
    delegatedSigner: key,
    accountIndex: 0,
    vault: key,
  },
  // @ts-expect-error Kamino markets are derived from RiskBasket.
  kaminoMarkets: [],
});

const config = LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta];
const settingsPda = deriveSquadsSettings(config, 1n);
const vaultPda = deriveSquadsVault(config, settingsPda.address, 0);
const innerInstruction = new TransactionInstruction({
  programId: key,
  keys: [{ pubkey: vaultPda.address, isSigner: false, isWritable: true }],
  data: Buffer.from([1]),
});

createSquadsSmartAccountInstruction(config, {
  payer: key,
  verifier: key,
  seed: 1n,
  treasury: key,
});
compileSquadsTransactionInstructions([innerInstruction]);
createSquadsSyncTransactionInstruction(config, {
  settings: settingsPda.address,
  signer: key,
  accountIndex: 0,
  instructions: [innerInstruction],
});
createSquadsProgramInteractionExecutionInstruction(config, {
  policy: key,
  signer: key,
  accountIndex: 0,
  instructions: [innerInstruction],
  instructionConstraintIndexes: [0, 1],
});
