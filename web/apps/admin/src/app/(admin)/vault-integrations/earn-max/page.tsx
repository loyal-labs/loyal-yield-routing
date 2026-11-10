import type { ReactNode } from "react";
import { useRouter } from "next/navigation";

import { AddressLink } from "@/components/blockchain/address-link";
import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { cn } from "@/lib/utils";

import { VaultDetails } from "./backyard-vault";
import { EarnMaxRefresh, TimeChart } from "./earn-max-charts";
import {
  type EarnMaxData,
  earnMaxData,
  type EarnMaxProps,
  type Part,
} from "./earn-max-data";

const STALE_REPORT_S = 75 * 60;

const ACTION_WORDS: Record<string, string> = {
  DELEVER_PRIME_USDC_STEP: "Repaid or withdrew on Kamino",
  DELEVER_ROUTE_STEP: "Repaid or withdrew on Kamino",
  INITIALIZE_KAMINO_OBLIGATION: "Opened a Kamino account",
  OPEN_PRIME_USDC_STEP: "Deposited or borrowed on Kamino",
  OPEN_ROUTE_STEP: "Deposited or borrowed on Kamino",
  POLICY_SETUP_CREATE: "Created a spending policy",
  POLICY_SETUP_PREFUND: "Funded a spending policy setup",
  RECOVER_TRANSACTION: "Recovered an unfinished transaction",
  STAGE_SQUADS_TO_VOLTR: "Moved worker cash back to the vault",
  SWAP_COLLATERAL_TO_DEBT_STEP: "Swapped collateral to the borrowed token",
  SWAP_COLLATERAL_TO_STABLE_STEP: "Swapped collateral to USDC",
  SWAP_DEBT_TO_COLLATERAL_STEP: "Swapped the borrowed token to collateral",
  SWAP_DEBT_TO_USDC_STEP: "Swapped leftover borrowed token to USDC",
  SWAP_PRIME_TO_USDC_STEP: "Swapped collateral to USDC",
  SWAP_STABLE_TO_COLLATERAL_STEP: "Swapped USDC to collateral",
  SWAP_USDC_TO_DEBT_STEP: "Swapped USDC to the borrowed token",
  SWAP_USDC_TO_PRIME_STEP: "Swapped USDC to collateral",
  VOLTR_ALLOCATE_TO_SQUADS: "Moved vault cash to the worker",
  VOLTR_RESTORE_IDLE: "Returned cash to the vault",
};

const SELECTOR_REASONS: Record<string, string> = {
  advantage_not_yet_persistent:
    "a better market has not lasted long enough yet",
  complete_current_tranche_first: "finishing the current step first",
  no_worthwhile_executable_move: "no better market right now",
  persistent_net_benefit: "a better market has lasted long enough",
};

const words = (value: string | null | undefined) =>
  value ? value.replaceAll("_", " ").toLowerCase() : "";

function usd(value: number | null | undefined) {
  if (value === null || value === undefined) return "—";
  return `${value < 0 ? "−" : ""}$${Math.abs(value).toLocaleString("en-US", {
    maximumFractionDigits: 2,
    minimumFractionDigits: 2,
  })}`;
}

function compactUsd(value: number) {
  return `$${new Intl.NumberFormat("en-US", {
    maximumFractionDigits: 1,
    notation: "compact",
  }).format(value)}`;
}

const pct = (value: number | null | undefined, digits = 2) =>
  value === null || value === undefined ? "—" : `${value.toFixed(digits)}%`;

function ago(seconds: number | null | undefined) {
  if (seconds === null || seconds === undefined) return "—";
  if (seconds < 90) return `${Math.round(seconds)} s`;
  if (seconds < 5400) return `${Math.round(seconds / 60)} min`;
  if (seconds < 172_800) return `${(seconds / 3600).toFixed(1)} h`;
  return `${(seconds / 86_400).toFixed(1)} d`;
}

const secondsSince = (iso: string | null | undefined, now: number) =>
  iso ? (now - Date.parse(iso)) / 1000 : null;

function dateTime(iso: string | null | undefined) {
  if (!iso) return "—";
  return `${new Intl.DateTimeFormat("en-US", {
    day: "numeric",
    hour: "2-digit",
    hour12: false,
    minute: "2-digit",
    month: "short",
    timeZone: "UTC",
  }).format(new Date(iso))} UTC`;
}

const levelLabel = (level: number) => `${level}x`;

export default function EarnMaxPage(props: EarnMaxProps) {
  const router = useRouter();
  const data = earnMaxData(props);
  const now = Date.parse(data.loadedAt);
  const route = data.route.ok ? data.route.value : null;
  const openLatches = data.latches.ok ? data.latches.value.open : null;
  const custodyUsd = data.balances.ok ? data.balances.value.cash : null;
  const ltv = route?.ltvRatio ?? null;
  const leverageNow = ltv !== null && ltv >= 0 && ltv < 1 ? 1 / (1 - ltv) : null;

  return (
    <PageContainer className="max-w-6xl space-y-6">
      <EarnMaxRefresh />
      <SectionHeader
        breadcrumbs={[{ label: "Vault integrations" }, { label: "Earn MAX vaults" }]}
        subtitle="Balances, positions and worker activity for the selected vault. Refreshes every minute."
        title="Earn MAX vaults"
      />
      <Tabs
        value={data.selectedVault.id}
        onValueChange={(vault) => router.push(`/vault-integrations/earn-max?vault=${vault}`)}
        activationMode="manual"
      >
        <TabsList aria-label="Vault" className="h-11">
          <TabsTrigger value="loyal">Loyal Earn MAX</TabsTrigger>
          <TabsTrigger value="backyard">Backyard</TabsTrigger>
        </TabsList>
        <TabsContent value={data.selectedVault.id} className="space-y-6 pt-4">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="space-y-1">
              <h2 className="text-xl font-semibold tracking-tight">{data.selectedVault.name}</h2>
              <p className="text-sm text-muted-foreground">Voltr vault <AddressLink address={data.selectedVault.address} /></p>
            </div>
            <p className="text-xs text-muted-foreground">Worker: <span className="font-mono">{data.selectedVault.worker}</span></p>
          </div>

          <Pulse openLatches={openLatches} route={route} />

          <section className="grid gap-4 lg:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
            <RightNow data={data} ltv={ltv} now={now} />
            <LoanSafety
              custodyUsd={custodyUsd}
              data={data}
              leverageNow={leverageNow}
              ltv={ltv}
            />
          </section>

          <ApyHistory data={data} />
          <LeverageLevels data={data} leverageNow={leverageNow} now={now} />
          <BorrowingRoom data={data} />
          <PositionHistory data={data} />
          <MoneyMoves data={data} />

          <section className="grid gap-4 lg:grid-cols-2">
            <WorkerHealth data={data} now={now} />
            <Latches data={data} now={now} />
          </section>

          <VaultDetails {...props.backyard} />
        </TabsContent>
      </Tabs>

      <p className="text-xs text-muted-foreground">
        Loaded {dateTime(data.loadedAt)}. Sources: Yield database, Solana RPC,
        Voltr and Kamino public APIs. Vault identity stays fixed when refreshing or sharing this URL.
      </p>
    </PageContainer>
  );
}

function shortError(message: string) {
  const text = message
    .split(/[{\n]|\. /)[0]
    .trim()
    .replace(/:$/, "");
  return text.length > 80 ? `${text.slice(0, 79)}…` : text || "unknown error";
}

function Unavailable({ part, what }: { part: Part<unknown>; what: string }) {
  if (part.ok) return null;
  return (
    <p className="rounded-md border border-dashed p-3 text-sm text-muted-foreground">
      {what} unavailable: {shortError(part.error)}
    </p>
  );
}

function Label({ children }: { children: ReactNode }) {
  return (
    <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
      {children}
    </p>
  );
}

function Fact({
  hint,
  label,
  value,
}: {
  hint: ReactNode;
  label: string;
  value: ReactNode;
}) {
  return (
    <div className="space-y-0.5">
      <Label>{label}</Label>
      <p className="text-lg font-semibold tabular-nums">{value}</p>
      <p className="text-xs text-muted-foreground">{hint}</p>
    </div>
  );
}

type Route = Extract<EarnMaxData["route"], { ok: true }>["value"];

function Pulse({
  openLatches,
  route,
}: {
  openLatches: Array<{ reason: string }> | null;
  route: Route | null;
}) {
  const latched = Boolean(openLatches?.length);
  const stale = (route?.reportAgeS ?? Infinity) > STALE_REPORT_S;

  return (
    <div className="flex flex-wrap items-center gap-2 text-sm">
      <Badge
        variant={latched || stale ? "destructive" : "outline"}
        title="Worker state"
      >
        {latched
          ? `Stopped: ${openLatches!.map((l) => words(l.reason)).join(", ")}`
          : route
          ? `Last NAV report ${ago(route.reportAgeS)} ago`
          : "Worker state unavailable"}
      </Badge>
      {route?.version ? (
        <Badge variant="secondary">Worker {route.version}</Badge>
      ) : null}
      {openLatches ? (
        <Badge variant="secondary">
          {openLatches.length === 0
            ? "No open latches"
            : `${openLatches.length} open latch${
                openLatches.length === 1 ? "" : "es"
              }`}
        </Badge>
      ) : null}
      {route?.reportObservedAt ? (
        <span className="text-xs text-muted-foreground">
          Routine NAV reports hourly · last report {dateTime(route.reportObservedAt)} · state written{" "}
          {ago(route.updatedAgeS)} ago
        </span>
      ) : null}
    </div>
  );
}

export function verdict(
  route: Route | null,
  latches: Array<{ reason: string }> | null,
  ltv: number | null
) {
  if (!route) return { bad: true, text: "Worker state is unavailable." };
  if (latches?.length)
    return { bad: true, text: "The worker is stopped and needs a clear." };
  if (latches === null) return { bad: true, text: "Safety holds could not be checked." };
  if (route.navUsd === null || route.aumUsd === null || ltv === null)
    return { bad: true, text: "Current valuation is unavailable." };
  if (route.updatedAgeS > 3 * 60) return { bad: true, text: "Worker observations are stale." };
  if ((route.reportAgeS ?? Infinity) > STALE_REPORT_S)
    return { bad: true, text: "NAV reports have stalled." };
  if (route.navFresh === false)
    return { bad: false, text: "Valuation available; NAV report update pending." };
  return { bad: false, text: "Reporting normally." };
}

function RightNow({
  data,
  ltv,
  now,
}: {
  data: EarnMaxData;
  ltv: number | null;
  now: number;
}) {
  const route = data.route.ok ? data.route.value : null;
  const latches = data.latches.ok ? data.latches.value.open : null;
  const health = data.health.ok ? data.health.value : null;
  const vault = data.vault.ok ? data.vault.value : null;
  const idle = data.balances.ok ? data.balances.value.idle : null;
  const earnedError = data.vault.ok ? data.vault.value.error : data.vault.error;
  const earnedReason = earnedError
    ? earnedError.includes("429")
      ? "RPC rate limit"
      : shortError(earnedError)
    : null;
  const earnedNote = !earnedReason
    ? null
    : vault
    ? `Earned figure from ${ago(
        secondsSince(vault.computedAt, now)
      )} ago; refresh failed: ${earnedReason}`
    : `Earned figure unavailable: ${earnedReason}`;
  const state = verdict(route, latches, ltv);
  const why = latches?.length
    ? `Open latch: ${latches
        .map((l) => words(l.reason))
        .join(", ")}. Nothing moves until it is cleared.`
    : [
        `Current route LTV ${ltv === null ? "—" : pct(ltv * 100, 1)}.`,
        health
          ? `${health.navReconciled} of ${health.navAll} NAV reports landed in the last 24 hours.`
          : "",
        route?.selectorAction && route.selectorReason !== "accounting_first"
          ? `Optimizer: ${words(route.selectorAction)} (${
              (route.selectorReason &&
                SELECTOR_REASONS[route.selectorReason]) ??
              words(route.selectorReason)
            }).`
          : "",
      ].join(" ");
  const apyNow = route?.currentApy;
  const apyAge = secondsSince(apyNow?.observedAt, now);
  const portfolio = apyNow?.portfolio && apyAge !== null && apyAge >= 0 && apyAge <= 15 * 60 && (route?.updatedAgeS ?? Infinity) <= 180
    ? apyNow.portfolio : null;
  const realized = data.apy.ok ? data.apy.value : null;
  const series = data.navSeries.ok ? data.navSeries.value : [];

  return (
    <Card>
      <CardHeader>
        <CardDescription>Right now</CardDescription>
        <CardTitle
          className={cn(
            "text-2xl tracking-tight",
            state.bad && "text-destructive"
          )}
        >
          {state.text}
        </CardTitle>
        <p className="text-sm text-muted-foreground">{why}</p>
      </CardHeader>
      <CardContent className="space-y-5">
        <div>
          <Label>Total vault value</Label>
          <div className="flex flex-wrap items-baseline gap-3">
            <p className="text-4xl font-semibold tabular-nums tracking-tight">
              {usd(route?.aumUsd)}
            </p>
            {vault ? (
              <p
                className="font-mono text-sm tabular-nums"
                title={`Holders' value ${usd(vault.valueUsd)} − deposits ${usd(
                  vault.deposits
                )} + withdrawals ${usd(vault.withdrawals)} (${
                  vault.flows
                } deposits and withdrawals on chain since ${dateTime(
                  vault.since
                )})`}
              >
                {vault.earned >= 0 ? "+" : "−"}$
                {Math.abs(vault.earned).toFixed(2)} earned
              </p>
            ) : null}
          </div>
          <p className="font-mono text-xs text-muted-foreground">
            {route?.navUsd != null
              ? `${usd(route.navUsd)} in the loop · ${
                  idle === null
                    ? "idle cash unavailable"
                    : idle > 0.005
                    ? `${usd(idle)} idle, not earning yet`
                    : "all of it earning"
                }`
              : null}
          </p>
          {earnedNote ? (
            <p className="text-xs text-muted-foreground">{earnedNote}</p>
          ) : null}
        </div>

        <div className="grid grid-cols-2 gap-4">
          <Fact
            hint={
              portfolio
                ? `All holdings and idle cash, after borrowing costs and performance fees · ${ago(apyAge)} ago`
                : "A fresh estimate covering every holding is unavailable"
            }
            label="Estimated vault APY"
            value={portfolio ? pct(portfolio.apyBps / 100) : "—"}
          />
          <Fact
            hint={
              realized
                ? `From the share price since ${realized.since}`
                : data.apy.ok
                ? "Needs 12 hours of history"
                : `Voltr unavailable: ${data.apy.error}`
            }
            label="Realized APY"
            value={realized ? pct(realized.apy * 100) : "—"}
          />
        </div>

        {portfolio ? (
          <div className="space-y-2">
            <Table aria-label="Vault APY breakdown">
              <TableHeader><TableRow>
                <TableHead>Holding</TableHead>
                <TableHead className="text-right">Net value</TableHead>
                <TableHead className="text-right">Position APY</TableHead>
              </TableRow></TableHeader>
              <TableBody>
                {portfolio.positions.map((position) => (
                  <TableRow key={position.lane}>
                    <TableCell>{position.lane}
                      {position.debtValueRaw > 0 ? <p className="text-xs text-muted-foreground">{usd(position.debtValueRaw / 1e6)} borrowed, deducted from value</p> : null}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{usd(position.equityRaw / 1e6)}</TableCell>
                    <TableCell className="text-right tabular-nums">{position.apyBps === null ? "—" : pct(position.apyBps / 100)}</TableCell>
                  </TableRow>
                ))}
                <TableRow>
                  <TableCell>Idle USDC</TableCell>
                  <TableCell className="text-right tabular-nums">{usd(portfolio.idleUsdcRaw / 1e6)}</TableCell>
                  <TableCell className="text-right tabular-nums">0.00%</TableCell>
                </TableRow>
              </TableBody>
            </Table>
            <p className="text-xs text-muted-foreground">Forward estimate at current rates on {usd(portfolio.totalValueRaw / 1e6)} of vault value. Future trading costs and rate changes are excluded; realized return comes from the share price.</p>
          </div>
        ) : null}

        <div className="space-y-1">
          <Label>Value in the loop, last 24 hours</Label>
          {series.length > 1 ? (
            <TimeChart
              data={series.map((point) => ({
                at: point.at,
                value: point.navUsd,
              }))}
              label="NAV"
              unit="usd"
            />
          ) : (
            <p className="text-sm text-muted-foreground">
              Not enough NAV reports to draw a chart.
            </p>
          )}
          <Unavailable part={data.navSeries} what="NAV history" />
        </div>
      </CardContent>
    </Card>
  );
}

function LoanSafety({
  custodyUsd, data, leverageNow, ltv,
}: {
  custodyUsd: number | null;
  data: EarnMaxData;
  leverageNow: number | null;
  ltv: number | null;
}) {
  const route = data.route.ok ? data.route.value : null;
  const target = route?.leverageTarget ?? null;
  const targetHint = target === null
    ? "No recorded target for this route"
    : leverageNow !== null && Math.abs(target - leverageNow) < 0.05
      ? "At target"
      : route?.upApproved === false && leverageNow !== null && target > leverageNow
        ? "Additional borrowing is not approved"
        : "Recorded target; execution depends on worker checks";

  return (
    <Card>
      <CardHeader>
        <CardDescription>Current route position</CardDescription>
        <CardTitle className="text-base">{route?.currentApy?.lane ?? "Kamino borrowing"}</CardTitle>
        <p className="text-sm text-muted-foreground">
          LTV is debt divided by collateral for this route. Borrowing limits vary by market;
          these figures do not describe every position in the vault.
        </p>
      </CardHeader>
      <CardContent className="space-y-5">
        <div className="grid grid-cols-2 gap-x-6 gap-y-4">
          <Fact hint="Observed by the worker" label="LTV" value={ltv === null ? "—" : pct(ltv * 100, 2)} />
          <Fact hint="Value in US dollars" label="Collateral" value={usd(route?.collateralUsd)} />
          <Fact hint="Debt value in US dollars" label="Borrowed" value={usd(route?.debtUsd)} />
          <Fact hint="Collateral / equity in this route" label="Leverage now" value={leverageNow === null ? "—" : `${leverageNow.toFixed(2)}x`} />
          <Fact hint={targetHint} label="Leverage target" value={target === null ? "—" : levelLabel(target)} />
          <Fact hint={<AddressLink address={data.selectedVault.cashAta} />} label="Worker USDC cash" value={usd(custodyUsd)} />
          <Fact hint={<AddressLink address={data.selectedVault.idleAta} />} label="Vault USDC cash" value={data.balances.ok ? usd(data.balances.value.idle) : "—"} />
        </div>
        <Unavailable part={data.balances} what="Cash balances" />
        <Unavailable part={data.route} what="Worker state" />
      </CardContent>
    </Card>
  );
}

function LeverageLevels({
  data,
  leverageNow,
  now,
}: {
  data: EarnMaxData;
  leverageNow: number | null;
  now: number;
}) {
  const watch = data.route.ok ? data.route.value.leverageWatch : null;
  const levels = watch
    ? [
        ...new Set(watch.lanes.flatMap((lane) => Object.keys(lane.apyPct))),
      ].sort((a, b) => Number(a) - Number(b))
    : [];
  const nowLevel =
    leverageNow === null
      ? null
      : levels.reduce<string | null>(
          (best, level) =>
            best === null ||
            Math.abs(Number(level) - leverageNow) <
              Math.abs(Number(best) - leverageNow)
              ? level
              : best,
          null
        );

  return (
    <Card>
      <CardHeader>
        <CardDescription>Leverage levels</CardDescription>
        <CardTitle className="text-base">
          What each market would pay at each leverage level
        </CardTitle>
        {watch ? (
          <p className="text-xs text-muted-foreground">
            Hourly worker reading from {dateTime(watch.observedAt)} (
            {ago(secondsSince(watch.observedAt, now))} ago)
          </p>
        ) : null}
      </CardHeader>
      <CardContent className="space-y-3">
        {!data.route.ok ? (
          <Unavailable part={data.route} what="Leverage readings" />
        ) : !watch ? (
          <p className="text-sm text-muted-foreground">
            Awaiting the first reading. The worker writes one every hour once
            the leverage watch is live.
          </p>
        ) : (
          <>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Market</TableHead>
                  <TableHead className="text-right">Spread</TableHead>
                  {levels.map((level) => (
                    <TableHead className="text-right" key={level}>
                      {levelLabel(Number(level))}
                      {level === nowLevel ? " (now)" : ""}
                    </TableHead>
                  ))}
                  <TableHead>Level picks</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {watch.lanes.map((lane) => {
                  const best = Math.max(...Object.values(lane.apyPct));
                  return (
                    <TableRow key={lane.lane}>
                      <TableCell>
                        <p className="font-medium">{lane.lane}</p>
                        <p className="text-xs text-muted-foreground">
                          {lane.enterable ? "Can enter" : "Cannot enter yet"}
                          {lane.older ? " · older reading" : ""}
                        </p>
                      </TableCell>
                      <TableCell
                        className={cn(
                          "text-right font-mono tabular-nums",
                          lane.spreadPct < 0 && "text-destructive"
                        )}
                      >
                        {lane.spreadPct > 0
                          ? "+"
                          : lane.spreadPct < 0
                          ? "−"
                          : ""}
                        {Math.abs(lane.spreadPct).toFixed(2)}
                      </TableCell>
                      {levels.map((level) => {
                        const apy = lane.apyPct[level];
                        return (
                          <TableCell
                            className={cn(
                              "text-right font-mono tabular-nums text-muted-foreground",
                              apy === best && "font-semibold text-foreground"
                            )}
                            key={level}
                          >
                            {apy === undefined ? "—" : pct(apy)}
                          </TableCell>
                        );
                      })}
                      <TableCell className="font-mono text-sm">
                        {lane.levels.map(levelLabel).join(" · ") || "—"}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
            <p className="text-xs text-muted-foreground">
              Spread is token yield minus borrow cost, in percentage points.
              Bold is the best level for each market. An older reading means the
              market was missing from the latest hourly reading. Level picks:
              the level the worker would choose under its three safety settings.
            </p>
          </>
        )}
      </CardContent>
    </Card>
  );
}

function BorrowingRoom({ data }: { data: EarnMaxData }) {
  return (
    <Card>
      <CardHeader>
        <CardDescription>Borrowing room</CardDescription>
        <CardTitle className="text-base">
          How much more each market can lend before Kamino blocks borrowing
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        {data.borrowing.ok ? (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Market</TableHead>
                <TableHead>Borrowed token</TableHead>
                <TableHead className="text-right">Pool used</TableHead>
                <TableHead className="text-right">Borrow APY</TableHead>
                <TableHead className="text-right">Room to 90% used</TableHead>
                <TableHead className="text-right">Pool size</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.borrowing.value.map((row) => (
                <TableRow key={row.lane}>
                  <TableCell className="font-medium">{row.lane}</TableCell>
                  <TableCell>{row.token}</TableCell>
                  <TableCell
                    className={cn(
                      "text-right font-mono tabular-nums",
                      row.utilization >= 0.9 && "text-destructive"
                    )}
                  >
                    {pct(row.utilization * 100, 1)}
                  </TableCell>
                  <TableCell className="text-right font-mono tabular-nums">
                    {pct(row.borrowApyPct)}
                  </TableCell>
                  <TableCell className="text-right font-mono tabular-nums">
                    {usd(row.roomTo90Usd)}
                  </TableCell>
                  <TableCell className="text-right font-mono tabular-nums text-muted-foreground">
                    {compactUsd(row.supplyUsd)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ) : (
          <Unavailable part={data.borrowing} what="Kamino metrics" />
        )}
        <p className="text-xs text-muted-foreground">
          From Kamino public reserve metrics. Pool used = borrowed / supplied.
          Above 90% used, Kamino stops new borrows.
        </p>
      </CardContent>
    </Card>
  );
}

function ApyHistory({ data }: { data: EarnMaxData }) {
  const history = data.apy.ok ? data.apy.value?.history ?? [] : [];
  const chart = (title: string, key: "dayApy" | "sevenDayApy") => (
    <div className="min-w-0 space-y-1">
      <Label>{title}</Label>
      <TimeChart
        data={history.map((p) => ({
          at: p.at,
          value: p[key] === null ? null : p[key] * 100,
        }))}
        label={title}
        unit="pct"
      />
    </div>
  );

  return (
    <Card>
      <CardHeader>
        <CardDescription>APY paid to users</CardDescription>
        <CardTitle className="text-base">
          Share-price growth after all costs, per day
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        {!data.apy.ok ? (
          <Unavailable part={data.apy} what="APY history" />
        ) : history.length < 2 ? (
          <p className="text-sm text-muted-foreground">
            Not enough days of history yet.
          </p>
        ) : (
          <div className="grid gap-6 md:grid-cols-2">
            {chart("Realized APY", "sevenDayApy")}
            {chart("Daily APY", "dayApy")}
          </div>
        )}
        <p className="text-xs text-muted-foreground">
          Annualized from the Voltr share price. The realized line aims for a
          week and uses the available history when dates are missing; the current
          figure shows its actual start date above. The daily line uses successive
          reported days. The estimated vault APY is a forecast at current rates.
        </p>
      </CardContent>
    </Card>
  );
}

function PositionHistory({ data }: { data: EarnMaxData }) {
  const points = data.history.ok ? data.history.value : [];
  const chart = (
    title: string,
    key: "equityUsd" | "ltvPct",
    unit: "pct" | "usd"
  ) => (
    <div className="min-w-0 space-y-1">
      <Label>{title}</Label>
      <TimeChart
        data={points.map((p) => ({ at: p.at, value: p[key] }))}
        label={title}
        unit={unit}
      />
    </div>
  );

  return (
    <Card>
      <CardHeader>
        <CardDescription>Position history</CardDescription>
        <CardTitle className="text-base">Last 7 days, hourly average</CardTitle>
      </CardHeader>
      <CardContent>
        {!data.history.ok ? (
          <Unavailable part={data.history} what="Position history" />
        ) : points.length < 2 ? (
          <p className="text-sm text-muted-foreground">
            Not enough snapshots in the last 7 days.
          </p>
        ) : (
          <div className="grid gap-6 md:grid-cols-2">
            {chart("Equity", "equityUsd", "usd")}
            {chart("LTV", "ltvPct", "pct")}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function moveLabel(move: { action: string; reason: string | null }) {
  if (move.action === "OPEN_ROUTE_STEP") {
    if (move.reason === "leverage_up") return "Borrowed on Kamino";
    if (move.reason?.includes("redeposit"))
      return "Deposited collateral on Kamino";
  }
  return ACTION_WORDS[move.action] ?? words(move.action);
}

function statusVariant(status: string) {
  if (status === "reconciled") return "outline" as const;
  if (status === "failed" || status === "manual_recovery")
    return "destructive" as const;
  return "secondary" as const;
}

function MoneyMoves({ data }: { data: EarnMaxData }) {
  return (
    <Card>
      <CardHeader>
        <CardDescription>Money moves</CardDescription>
        <CardTitle className="text-base">
          Last 30 worker steps that reached the chain, newest first
        </CardTitle>
      </CardHeader>
      <CardContent>
        {!data.moves.ok ? (
          <Unavailable part={data.moves} what="Money moves" />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Time</TableHead>
                <TableHead>What it did</TableHead>
                <TableHead className="text-right">Amount (raw units)</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Transaction</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.moves.value.length ? (
                data.moves.value.map((move, index) => (
                  <TableRow key={`${move.at}-${index}`}>
                    <TableCell className="whitespace-nowrap text-sm">
                      {dateTime(move.at)}
                    </TableCell>
                    <TableCell>
                      <p>{moveLabel(move)}</p>
                      <p className="max-w-[28rem] truncate text-xs text-muted-foreground">
                        {[move.strategyKey, words(move.reason)]
                          .filter(Boolean)
                          .join(" · ")}
                      </p>
                    </TableCell>
                    <TableCell className="text-right font-mono tabular-nums">
                      {move.amountRaw && Number(move.amountRaw) > 0
                        ? BigInt(move.amountRaw).toLocaleString("en-US")
                        : "—"}
                    </TableCell>
                    <TableCell>
                      <Badge variant={statusVariant(move.status)}>
                        {words(move.status)}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      {move.signature ? (
                        <a
                          className="font-mono text-xs underline underline-offset-2"
                          href={`https://solscan.io/tx/${move.signature}`}
                          rel="noreferrer"
                          target="_blank"
                        >
                          {move.signature.slice(0, 8)}…
                        </a>
                      ) : (
                        "—"
                      )}
                    </TableCell>
                  </TableRow>
                ))
              ) : (
                <TableRow>
                  <TableCell className="text-muted-foreground" colSpan={5}>
                    No money moves recorded yet.
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        )}
        <p className="mt-2 text-xs text-muted-foreground">
          Amounts are raw token units; decimals vary by token. Open the transaction
          for token amounts. Hold decisions and NAV reports are left out.
        </p>
        {data.health.ok ? (
          <p className="text-xs text-muted-foreground">
            {data.health.value.refusedBeforeSending} refused before sending in
            the last 24 h.
          </p>
        ) : null}
      </CardContent>
    </Card>
  );
}

function WorkerHealth({ data, now }: { data: EarnMaxData; now: number }) {
  const route = data.route.ok ? data.route.value : null;
  const health = data.health.ok ? data.health.value : null;

  return (
    <Card>
      <CardHeader>
        <CardDescription>Worker health</CardDescription>
        <CardTitle className="text-base">Last 24 hours</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4 text-sm">
        <div className="grid grid-cols-2 gap-x-6 gap-y-4">
          <Fact
            hint={
              route?.workerOwner
                ? `Last observed ${ago(route.updatedAgeS)} ago`
                : "Release not recorded by this worker"
            }
            label="Last reporting worker"
            value={route?.version ?? "—"}
          />
          <Fact
            hint={
              route?.currentOperationId
                ? "A step is in flight"
                : `Route ${words(route?.routeStatus) || "status unknown"}`
            }
            label="State last written"
            value={route ? `${ago(route.updatedAgeS)} ago` : "—"}
          />
          <Fact
            hint={
              route?.reportSlot
                ? `Slot ${route.reportSlot.toLocaleString("en-US")}${
                    route.navFresh === false ? " · update pending" : ""
                  }`
                : "—"
            }
            label="Last NAV report"
            value={
              route
                ? `${ago(secondsSince(route.reportObservedAt, now))} ago`
                : "—"
            }
          />
          <Fact
            hint={
              health
                ? `${health.navRefusedBeforeSending} refused before sending · ${
                    health.navFailed - health.navRefusedBeforeSending
                  } signed attempts failed · ${
                    health.navAll - health.navReconciled - health.navFailed
                  } other`
                : "—"
            }
            label="NAV reports landed"
            value={health ? `${health.navReconciled} of ${health.navAll}` : "—"}
          />
        </div>
        {health ? (
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1">
              <Label>Why NAV attempts did not land</Label>
              {health.navFailureReasons.length ? (
                health.navFailureReasons.map((row) => (
                  <p className="flex justify-between gap-3" key={`${row.reason}:${row.beforeSending}`}>
                    <span>
                      {row.reason === "hold:stale"
                        ? "Freshness check refused"
                        : row.reason === "hold:chain_refused"
                          ? "Transaction preparation refused"
                          : words(row.reason)}
                      {row.beforeSending ? " · not sent" : ""}
                    </span>
                    <span className="font-mono tabular-nums">{row.count}</span>
                  </p>
                ))
              ) : (
                <p className="text-muted-foreground">No failures.</p>
              )}
            </div>
            <div className="space-y-1">
              <Label>Failed money steps</Label>
              {health.failedSteps.length ? (
                health.failedSteps.map((row) => (
                  <p className="flex justify-between gap-3" key={row.action}>
                    <span>{ACTION_WORDS[row.action] ?? words(row.action)}</span>
                    <span className="font-mono tabular-nums">{row.count}</span>
                  </p>
                ))
              ) : (
                <p className="text-muted-foreground">No failures.</p>
              )}
            </div>
          </div>
        ) : null}
        <Unavailable part={data.health} what="Worker journal" />
      </CardContent>
    </Card>
  );
}

function Latches({ data, now }: { data: EarnMaxData; now: number }) {
  const duration = (from: string | null, to: string | null) =>
    from && to ? ago((Date.parse(to) - Date.parse(from)) / 1000) : "—";

  return (
    <Card>
      <CardHeader>
        <CardDescription>Latches</CardDescription>
        <CardTitle className="text-base">
          Stops that need a person to clear
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4 text-sm">
        {!data.latches.ok ? (
          <Unavailable part={data.latches} what="Latches" />
        ) : (
          <>
            <div className="space-y-1">
              <Label>Open</Label>
              {data.latches.value.open.length ? (
                data.latches.value.open.map((latch) => (
                  <p
                    className="flex justify-between gap-3 text-destructive"
                    key={latch.latchedAt}
                  >
                    <span>{words(latch.reason)}</span>
                    <span className="font-mono">
                      for {ago(secondsSince(latch.latchedAt, now))}
                    </span>
                  </p>
                ))
              ) : (
                <p className="text-muted-foreground">
                  No manual recovery holds are open.
                </p>
              )}
            </div>
            <div className="space-y-1">
              <Label>Last 10 cleared</Label>
              {data.latches.value.cleared.length ? (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Cleared</TableHead>
                      <TableHead>Stop</TableHead>
                      <TableHead>How it was cleared</TableHead>
                      <TableHead className="text-right">Lasted</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {data.latches.value.cleared.map((latch) => (
                      <TableRow key={latch.clearedAt}>
                        <TableCell className="whitespace-nowrap">
                          {dateTime(latch.clearedAt)}
                        </TableCell>
                        <TableCell>{words(latch.reason)}</TableCell>
                        <TableCell className="max-w-[14rem] whitespace-normal text-muted-foreground">
                          <span
                            className="line-clamp-2"
                            title={latch.clearedReason ?? undefined}
                          >
                            {latch.clearedReason ?? "—"}
                          </span>
                        </TableCell>
                        <TableCell className="text-right font-mono">
                          {duration(latch.latchedAt, latch.clearedAt)}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              ) : (
                <p className="text-muted-foreground">None cleared yet.</p>
              )}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
