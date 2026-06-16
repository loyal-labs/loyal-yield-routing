import { PublicKey, TransactionInstruction } from "@solana/web3.js";
import {
  LOYAL_CLUSTER_CONFIGS,
  LoyalCluster,
  RiskBasket,
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
