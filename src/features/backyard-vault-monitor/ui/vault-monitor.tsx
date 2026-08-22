import { BACKYARD_VAULT } from "../config";
import {
  buildBalanceSeries,
  calculateVaultApy,
  loadReserveRates,
  loadVaultHistory,
  loadVaultSnapshot,
} from "../server/vault-monitor.server";
import type { VaultApy, VaultHistory, VaultSnapshot } from "../types";
import { RefreshControl } from "./refresh-control";
import { VaultBalanceChart } from "./vault-balance-chart";

const USDC_SCALE = 1_000_000;

function formatUsdc(raw: bigint): string {
  if (raw > 0n && raw < 10_000n) return "<$0.01";
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(Number(raw) / USDC_SCALE);
}

function formatPercent(value: number): string {
  return new Intl.NumberFormat("en-US", {
    style: "percent",
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value);
}

function shortAddress(address: string): string {
  return `${address.slice(0, 5)}…${address.slice(-5)}`;
}

function ErrorMetric({ label, detail }: { label: string; detail: string }) {
  return (
    <article className="metric-card metric-error">
      <span className="metric-label">{label}</span>
      <strong className="metric-value">Unavailable</strong>
      <span className="metric-detail">{detail}</span>
    </article>
  );
}

function Metric({
  label,
  value,
  detail,
  accent,
}: {
  label: string;
  value: string;
  detail: string;
  accent?: "positive" | "negative";
}) {
  return (
    <article className="metric-card">
      <span className="metric-label">{label}</span>
      <strong className={`metric-value${accent ? ` metric-${accent}` : ""}`}>{value}</strong>
      <span className="metric-detail">{detail}</span>
    </article>
  );
}

async function settle<T>(promise: Promise<T>): Promise<{ value: T | null; error: string | null }> {
  try {
    return { value: await promise, error: null };
  } catch (error) {
    return { value: null, error: error instanceof Error ? error.message : "unknown data error" };
  }
}

export async function BackyardVaultMonitor() {
  const [snapshotResult, historyResult, ratesResult] = await Promise.all([
    settle(loadVaultSnapshot()),
    settle(loadVaultHistory()),
    settle(loadReserveRates()),
  ]);
  const snapshot = snapshotResult.value as VaultSnapshot | null;
  const history = historyResult.value as VaultHistory | null;
  let apy: VaultApy | null = null;
  let apyError = ratesResult.error;
  if (snapshot && ratesResult.value) {
    try {
      apy = calculateVaultApy(snapshot, ratesResult.value);
    } catch (error) {
      apyError = error instanceof Error ? error.message : "APY calculation failed";
    }
  } else if (!snapshot) {
    apyError = "current allocation is unavailable";
  }
  const balanceSeries = snapshot && history
    ? buildBalanceSeries(history, snapshot.totalValueRaw)
    : null;
  const observedAt = snapshot?.observedAt ?? new Date().toISOString();

  return (
    <section className="vault-monitor" aria-labelledby="vault-monitor-title">
      <header className="monitor-header">
        <div>
          <div className="eyebrow">Vault monitoring</div>
          <div className="title-row">
            <h1 id="vault-monitor-title">{BACKYARD_VAULT.name}</h1>
            <span className="network-pill">Mainnet</span>
          </div>
          <p className="monitor-subtitle">
            Voltr vault managed by Loyal&apos;s policy-constrained four-market Kamino router.
          </p>
        </div>
        <RefreshControl observedAt={observedAt} />
      </header>

      <div className="identity-strip">
        <a
          href={`https://solscan.io/account/${BACKYARD_VAULT.address}`}
          target="_blank"
          rel="noreferrer"
        >
          Vault <span>{shortAddress(BACKYARD_VAULT.address)}</span>
        </a>
        <a
          href={`https://solscan.io/token/${BACKYARD_VAULT.lpMint}`}
          target="_blank"
          rel="noreferrer"
        >
          LP mint <span>{shortAddress(BACKYARD_VAULT.lpMint)}</span>
        </a>
        <span>10 min withdrawal wait</span>
        <span>$1M cap</span>
      </div>

      <div className="metrics-grid">
        {snapshot ? (
          <Metric
            label="Current balance"
            value={formatUsdc(snapshot.totalValueRaw)}
            detail={`${formatUsdc(snapshot.idleRaw)} idle · slot ${snapshot.contextSlot.toLocaleString("en-US")}`}
          />
        ) : (
          <ErrorMetric label="Current balance" detail={snapshotResult.error ?? "RPC unavailable"} />
        )}
        {history ? (
          <Metric
            label="30d deposits"
            value={formatUsdc(history.depositsRaw)}
            detail={`${history.flows.filter((flow) => flow.kind === "deposit").length} confirmed deposit event${history.flows.filter((flow) => flow.kind === "deposit").length === 1 ? "" : "s"}`}
            accent="positive"
          />
        ) : (
          <ErrorMetric label="30d deposits" detail={historyResult.error ?? "history unavailable"} />
        )}
        {history ? (
          <Metric
            label="30d withdrawals"
            value={formatUsdc(history.withdrawalsRaw)}
            detail={`${history.flows.filter((flow) => flow.kind === "withdrawal").length} confirmed withdrawal event${history.flows.filter((flow) => flow.kind === "withdrawal").length === 1 ? "" : "s"}`}
            accent="negative"
          />
        ) : (
          <ErrorMetric label="30d withdrawals" detail={historyResult.error ?? "history unavailable"} />
        )}
        {apy ? (
          <Metric
            label="Estimated vault APY"
            value={formatPercent(apy.netSupplyApy)}
            detail={`${formatPercent(apy.grossSupplyApy)} gross · 5% performance fee`}
          />
        ) : (
          <ErrorMetric label="Estimated vault APY" detail={apyError ?? "market rates unavailable"} />
        )}
      </div>

      <div className="monitor-grid">
        <article className="panel chart-panel">
          <div className="panel-heading">
            <div>
              <span className="panel-kicker">Vault balance</span>
              <h2>Last 30 days</h2>
            </div>
            {history && <span className="panel-meta">{history.scannedSignatureCount} transactions scanned</span>}
          </div>
          {balanceSeries ? (
            <>
              <VaultBalanceChart points={balanceSeries} />
              <div className="chart-legend">
                <span><i className="legend-line" /> Total value</span>
                <span>Daily points follow confirmed Voltr user-flow events; the last point is live vault accounting.</span>
              </div>
            </>
          ) : (
            <div className="panel-empty">Balance history needs both current RPC state and confirmed event history.</div>
          )}
        </article>

        <article className="panel allocation-panel">
          <div className="panel-heading">
            <div>
              <span className="panel-kicker">Allocation</span>
              <h2>Current positions</h2>
            </div>
          </div>
          {snapshot ? (
            <div className="allocation-list">
              {[
                { id: "idle", label: "Idle USDC", valueRaw: snapshot.idleRaw },
                ...snapshot.positions,
              ].map((position, index) => {
                const share = snapshot.totalValueRaw === 0n
                  ? 0
                  : Number(position.valueRaw) / Number(snapshot.totalValueRaw);
                return (
                  <div className="allocation-row" key={position.id}>
                    <div className="allocation-name">
                      <i style={{ "--allocation-index": index } as React.CSSProperties} />
                      <span>{position.label}</span>
                    </div>
                    <div className="allocation-value">
                      <strong>{formatUsdc(position.valueRaw)}</strong>
                      <span>{formatPercent(share)}</span>
                    </div>
                    <div className="allocation-track">
                      <div style={{ width: `${Math.min(share * 100, 100)}%` }} />
                    </div>
                  </div>
                );
              })}
            </div>
          ) : (
            <div className="panel-empty">Allocation is unavailable until the confirmed account snapshot succeeds.</div>
          )}
          <p className="apy-note">
            APY is the position-weighted Kamino supply APY after the 5% performance fee. Idle USDC earns 0%; token incentives are not included.
          </p>
        </article>
      </div>
    </section>
  );
}
