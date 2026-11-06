import { AddressLink } from "@/components/blockchain/address-link";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

import type { BackyardVaultProps } from "./backyard-vault-data";

function amount(raw: string | null, decimals: number | null, symbol: string) {
  if (raw === null || decimals === null) return "—";
  return `${(Number(raw) / 10 ** decimals).toLocaleString("en-US", {
    maximumFractionDigits: 6,
  })} ${symbol}`;
}

export function VaultDetails(data: BackyardVaultProps) {
  if (!data.available) {
    return <p className="rounded-md border border-dashed p-3 text-sm text-muted-foreground">Position details unavailable: {data.error}</p>;
  }
  const position = data.currentPosition;
  return (
    <section className="grid gap-4 lg:grid-cols-2" aria-label="Position and accounts">
      <Card>
        <CardHeader>
          <CardDescription>Current route tokens</CardDescription>
          <CardTitle className="text-base">{position.strategy ?? "No open position"}</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <dl className="grid grid-cols-2 gap-4 text-sm">
            <div className="space-y-1">
              <dt className="text-muted-foreground">Collateral</dt>
              <dd className="font-medium tabular-nums">{amount(position.collateralRaw, position.collateralDecimals, position.collateralSymbol)}</dd>
            </div>
            <div className="space-y-1">
              <dt className="text-muted-foreground">Debt</dt>
              <dd className="font-medium tabular-nums">{amount(position.debtRaw, position.debtDecimals, position.debtSymbol)}</dd>
            </div>
            <div className="space-y-1">
              <dt className="text-muted-foreground">Strategy USDC cash</dt>
              <dd className="font-medium tabular-nums">{amount(data.strategyIdleRaw, 6, "USDC")}</dd>
            </div>
          </dl>
          {data.tokenError ? <p className="text-xs text-muted-foreground">Token amounts unavailable: {data.tokenError}</p> : null}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardDescription>Selected vault accounts</CardDescription>
          <CardTitle className="text-base">Voltr and Squads</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="space-y-3 text-sm">
            {data.settings.map(({ key, value }) => (
              <div className="flex flex-wrap items-center justify-between gap-2" key={key}>
                <dt className="text-muted-foreground">{key}</dt>
                <dd><AddressLink address={value} /></dd>
              </div>
            ))}
          </dl>
        </CardContent>
      </Card>
    </section>
  );
}
