import { ArrowUpRight } from "lucide-react";
import { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";

import "./app/globals.css";

import {
  chartBoxClass,
  EarnAumBarChart,
  type WeeklyChartPoint,
} from "./components/earn-aum-bar-chart";
import { MetricHelpTooltip } from "./components/metric-help-tooltip";

type EarnStats = {
  aumUsd: number;
  aumSeries: WeeklyChartPoint[];
  optimizationVolumeUsd: number;
  refreshedAt: string;
  totalUsers: number;
};

type Load = { state: "loading" } | { state: "failed" } | { state: "ready"; stats: EarnStats };

const usd = new Intl.NumberFormat("en-US", {
  currency: "USD",
  maximumFractionDigits: 2,
  minimumFractionDigits: 2,
  style: "currency",
});

const users = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 });

const pacific = new Intl.DateTimeFormat("en-US", {
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
  month: "short",
  timeZone: "America/Los_Angeles",
});

const metrics = [
  {
    label: "Earn AUM",
    value: (stats: EarnStats) => usd.format(stats.aumUsd),
    detail: "Current normalized Earn AUM.",
    tooltip: "Current value held in our active Earn routing policies.",
  },
  {
    label: "Optimization Volume",
    value: (stats: EarnStats) => usd.format(stats.optimizationVolumeUsd),
    detail: "Cumulative confirmed moved volume.",
    tooltip:
      "Total USDC reallocated by confirmed Earn optimizations. This measures routing throughput across reserves, so the same deposited dollar can add to volume again when it is moved by a later optimization.",
    },
  {
    label: "Total Users",
    value: (stats: EarnStats) => users.format(stats.totalUsers),
    detail: "Registered Loyal users.",
    tooltip: "Total wallet-based user accounts registered with Loyal.",
  },
];

function DashboardHome() {
  const [load, setLoad] = useState<Load>({ state: "loading" });

  useEffect(() => {
    fetch("/api/earn/stats")
      .then((response) => {
        if (!response.ok) throw new Error(`earn stats ${response.status}`);
        return response.json() as Promise<EarnStats>;
      })
      .then((stats) => setLoad({ state: "ready", stats }))
      .catch(() => setLoad({ state: "failed" }));
  }, []);

  return (
    <main className="min-h-screen bg-background text-foreground">
      <section className="border-b bg-card">
        <div className="mx-auto flex w-full max-w-7xl flex-col gap-7 px-6 py-8 lg:px-10">
          <header className="flex flex-col gap-5 md:flex-row md:items-center md:justify-between">
            <div className="flex items-center gap-3">
              <img
                alt=""
                aria-hidden="true"
                className="size-14 shrink-0"
                height={56}
                src="/app-icon.svg"
                width={56}
              />
              <div>
                <p className="text-sm font-medium text-muted-foreground">
                  Loyal
                </p>
                <h1 className="text-3xl font-semibold tracking-normal">
                  Performance Dashboard
                </h1>
              </div>
            </div>
            <a
              className="inline-flex h-12 w-fit items-center justify-center gap-2 rounded-full bg-primary px-5 text-base font-medium leading-5 text-primary-foreground transition-[background,transform] duration-150 ease-out hover:-translate-y-0.5 hover:bg-[#e72f34] focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary active:translate-y-0"
              href="https://askloyal.com/app"
              rel="noopener noreferrer"
              target="_blank"
            >
              Open app
              <ArrowUpRight aria-hidden="true" className="size-5" />
            </a>
          </header>

          {load.state === "failed" ? (
            <p className="border-t pt-7 text-center text-sm text-muted-foreground">
              Stats are unavailable right now.
            </p>
          ) : (
            <div className="grid gap-x-8 gap-y-6 border-t pt-7 md:grid-cols-3">
              {metrics.map((metric) => (
                <div className="min-w-0 text-center" key={metric.label}>
                  <div className="flex min-w-0 items-center justify-center gap-2">
                    <p className="min-w-0 text-sm font-medium text-muted-foreground">
                      {metric.label}
                    </p>
                    <MetricHelpTooltip
                      ariaLabel={`About ${metric.label}`}
                      tooltip={metric.tooltip}
                    />
                  </div>
                  {load.state === "ready" ? (
                    <p className="mt-2 text-3xl font-semibold tracking-normal">
                      {metric.value(load.stats)}
                    </p>
                  ) : (
                    <div className="mx-auto mt-2 h-9 w-48 animate-pulse rounded-md bg-muted" />
                  )}
                  <p className="mt-1 text-sm leading-6 text-muted-foreground">
                    {metric.detail}
                  </p>
                </div>
              ))}
            </div>
          )}
        </div>
      </section>

      {load.state === "failed" ? null : (
        <section className="mx-auto w-full max-w-7xl px-6 py-12 lg:px-10">
          <div className="min-w-0">
            <div className="flex flex-col gap-1">
              <h2 className="text-xl font-semibold tracking-normal">
                Earn AUM by Week
              </h2>
              {load.state === "ready" ? (
                <p className="text-sm leading-6 text-muted-foreground">
                  Weekly snapshot. Red is this week so far.{" "}
                  <time dateTime={load.stats.refreshedAt}>
                    Updated {pacific.format(new Date(load.stats.refreshedAt))} PT.
                  </time>
                </p>
              ) : (
                <div className="my-1 h-4 w-72 max-w-full animate-pulse rounded bg-muted" />
              )}
            </div>
            {load.state === "ready" ? (
              <EarnAumBarChart
                ariaLabel="Earn AUM weekly snapshot bar chart from Jun 15 through the latest data"
                metricLabel="Earn AUM"
                points={load.stats.aumSeries}
              />
            ) : (
              <div className={`${chartBoxClass} animate-pulse rounded-lg bg-muted`} />
            )}
          </div>
        </section>
      )}
    </main>
  );
}

createRoot(document.getElementById("root")!).render(<DashboardHome />);
