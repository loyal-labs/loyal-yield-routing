import type { Pages } from "@/api.gen/pages";

import { apyHistory, earnedSinceEmpty, realizedApy } from "./earn-max-math";

export type EarnMaxProps = Pages["/vault-integrations/earn-max"];

export const IDLE_ATA = "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh";
export const CUSTODY = {
  debt: "J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn",
  collateral: "9tDh95ofQ7B83bHAou1XJGJNnfRTs3hLfX8uyKW6u97G",
} as const;

export type Part<T> = { ok: true; value: T } | { ok: false; error: string };

function part<T>(source: {
  ok: boolean;
  value?: T | null;
  error?: string | null;
}): Part<T> {
  return source.ok
    ? { ok: true, value: source.value as T }
    : { ok: false, error: source.error ?? "unknown error" };
}

function mapPart<T, U>(source: Part<T>, convert: (value: T) => U): Part<U> {
  return source.ok
    ? { ok: true, value: convert(source.value) }
    : source;
}

export function earnMaxData(props: EarnMaxProps) {
  const now = Date.parse(props.loadedAt);

  return {
    apy: mapPart(part(props.apy), ({ daily, sharePrice }) => {
      const realized = realizedApy(daily, sharePrice / 1000, now);
      return (
        realized && {
          ...realized,
          history: apyHistory(daily, sharePrice / 1000, now),
        }
      );
    }),
    balances: part(props.balances),
    borrowing: part(props.borrowing),
    health: part(props.health),
    history: part(props.history),
    latches: part(props.latches),
    loadedAt: props.loadedAt,
    moves: part(props.moves),
    navSeries: part(props.navSeries),
    route: part(props.route),
    vault: mapPart(part(props.vault), ({ computedAt, error, flows, valueUsd }) => ({
      error,
      computedAt,
      valueUsd,
      ...earnedSinceEmpty(flows, valueUsd),
    })),
  };
}

export type EarnMaxData = ReturnType<typeof earnMaxData>;
