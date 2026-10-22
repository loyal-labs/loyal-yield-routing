import type { Pages } from "@/api.gen/pages";

export type BackyardVaultProps = Pages["/vault-integrations/earn-max"]["backyard"];

type OperationRow = BackyardVaultProps["history"][number];
type VaultSettings = BackyardVaultProps["settings"];

export type BackyardVaultData =
  | { available: false; error: string; observedAt: string }
  | {
      available: true;
      aumUsdMicros: bigint | null;
      currentPosition: {
        collateralRaw: bigint | null;
        debtRaw: bigint | null;
        ltvBps: bigint | null;
        strategy: string | null;
      };
      history: OperationRow[];
      navUsdMicros: bigint | null;
      observedAt: string;
      projectedApyBps: bigint | null;
      report: {
        fresh: boolean | null;
        observedAt: string | null;
        slot: bigint | null;
      };
      routeStatus: string | null;
      settings: VaultSettings;
      squadsIdleRaw: bigint | null;
      strategyIdleRaw: bigint | null;
      voltrIdleRaw: bigint | null;
      vaultCapRaw: bigint;
      withdrawalWaitSeconds: number;
    };

const toBigInt = (value: string | null) =>
  value === null ? null : BigInt(value);

export function backyardVaultData(props: BackyardVaultProps): BackyardVaultData {
  if (!props.available) {
    return {
      available: false,
      error: props.error,
      observedAt: props.observedAt,
    };
  }

  return {
    available: true,
    aumUsdMicros: toBigInt(props.aumUsdMicros),
    currentPosition: {
      collateralRaw: toBigInt(props.currentPosition.collateralRaw),
      debtRaw: toBigInt(props.currentPosition.debtRaw),
      ltvBps: toBigInt(props.currentPosition.ltvBps),
      strategy: props.currentPosition.strategy,
    },
    history: props.history,
    navUsdMicros: toBigInt(props.navUsdMicros),
    observedAt: props.observedAt,
    projectedApyBps: toBigInt(props.projectedApyBps),
    report: {
      fresh: props.report.fresh,
      observedAt: props.report.observedAt,
      slot: toBigInt(props.report.slot),
    },
    routeStatus: props.routeStatus,
    settings: props.settings,
    squadsIdleRaw: toBigInt(props.squadsIdleRaw),
    strategyIdleRaw: toBigInt(props.strategyIdleRaw),
    voltrIdleRaw: toBigInt(props.voltrIdleRaw),
    vaultCapRaw: BigInt(props.vaultCapRaw),
    withdrawalWaitSeconds: props.withdrawalWaitSeconds,
  };
}
