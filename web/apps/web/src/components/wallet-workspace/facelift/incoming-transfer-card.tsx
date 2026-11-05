"use client";

import { type ReactNode, useId } from "react";

import {
  ScrambleText,
  useBalanceVisibility,
} from "@/components/wallet-workspace/facelift/balance-visibility";
import { usePublicEnv } from "@/contexts/public-env-context";
import { openTrackedLink } from "@/lib/core/analytics";

import { relayExplorer } from "./bridge-move";
import { BridgeIcon } from "./bridge-pieces";
import { ThemedIcon } from "./themed-icon";
import {
  fromRaw,
  type IncomingItem,
  type IncomingTransfersState,
} from "./use-bridge-move";

const BASE = "/wallet-workspace/facelift";
const PLACEHOLDER = "asset-placeholder.svg";
const PILL =
  "flex min-w-16 shrink-0 items-center justify-center rounded-[40px] bg-foreground px-4 py-2.5 font-medium text-[13px] text-background leading-4 transition-colors enabled:hover:bg-foreground/90 disabled:opacity-40";
const STATUS =
  "flex shrink-0 items-center gap-1.5 text-[13px] text-muted-foreground leading-4";

function amountLabel({ amountRaw, decimals, symbol }: IncomingItem) {
  const [whole, fraction = ""] = fromRaw(amountRaw, decimals).split(".");
  return `+${whole}.${fraction.padEnd(2, "0")} ${symbol}`;
}

function Spinner() {
  return (
    <svg
      aria-hidden
      className="size-4 animate-spin motion-reduce:animate-none"
      fill="none"
      viewBox="0 0 16 16"
    >
      <circle cx="8" cy="8" r="6" stroke="currentColor" strokeOpacity="0.25" strokeWidth="2" />
      <path d="M14 8a6 6 0 0 0-6-6" stroke="currentColor" strokeLinecap="round" strokeWidth="2" />
    </svg>
  );
}

function announcement(item: IncomingItem, what: string) {
  const { status } = item;
  if (status.kind === "bridging") return `Bridging ${what}`;
  if (status.kind === "arrived") return `${what} arrived`;
  if (status.kind === "failed") return `Failed. ${status.message}`;
  return "";
}

export function IncomingTransferCard({
  item,
  onConfirm,
}: {
  item: IncomingItem;
  onConfirm: () => void;
}) {
  const publicEnv = usePublicEnv();
  const { isBalanceHidden } = useBalanceVisibility();
  const titleId = useId();
  const { status } = item;
  const what = `${fromRaw(item.amountRaw, item.decimals)} ${item.symbol} from ${item.shortName}`;
  const underMinimum =
    item.amountRaw < BigInt(item.minUsd) * BigInt(10) ** BigInt(item.decimals);
  const explorer =
    status.kind === "failed" && status.requestId
      ? relayExplorer(status.requestId)
      : null;

  return (
    <div className="flex w-full items-center rounded-[20px] bg-accent px-4">
      <div className="flex shrink-0 items-center py-2 pr-3">
        <span className="relative flex">
          <BridgeIcon file={item.icon ?? PLACEHOLDER} />
          <span className="-right-0.5 -bottom-0.5 absolute flex size-[18px] items-center justify-center rounded-full border-2 border-card bg-tertiary">
            <ThemedIcon
              className="size-2.5 text-white"
              src={`${BASE}/sidebar/receive.svg`}
            />
          </span>
        </span>
      </div>
      <div className="flex min-w-0 flex-1 flex-col gap-0.5 pt-3 pb-2.5">
        {status.kind === "failed" ? (
          <p className="text-[13px] text-muted-foreground leading-4" id={titleId}>
            {status.message}
            {explorer ? (
              <>
                {" "}
                <button
                  className="underline underline-offset-2 transition-colors hover:text-foreground"
                  onClick={() =>
                    openTrackedLink(publicEnv, {
                      href: explorer,
                      linkText: "View in Explorer",
                      source: "incoming_transfer_failed",
                    })
                  }
                  type="button"
                >
                  View in Explorer
                </button>
              </>
            ) : null}
          </p>
        ) : (
          <p className="truncate text-[13px] text-muted-foreground leading-4" id={titleId}>
            {status.kind === "waiting" && underMinimum
              ? `Under the $${item.minUsd} minimum`
              : "Confirm incoming transfer"}
          </p>
        )}
        <p className="flex min-w-0 flex-wrap gap-x-[0.25em] font-medium text-[16px] text-foreground leading-5 tracking-[-0.176px]">
          <span className="min-w-0 max-w-full truncate">
            <ScrambleText isHidden={isBalanceHidden} text={amountLabel(item)} />
          </span>
          <span className="whitespace-nowrap text-tertiary">
            from {item.shortName}
          </span>
        </p>
      </div>
      <div className="ml-3 flex shrink-0 items-center gap-3">
        {status.kind === "waiting" ? (
          <button
            aria-describedby={underMinimum ? titleId : undefined}
            aria-label={`Confirm ${what}`}
            className={PILL}
            disabled={underMinimum}
            onClick={onConfirm}
            type="button"
          >
            Confirm
          </button>
        ) : status.kind === "bridging" ? (
          <span className={STATUS}>
            <Spinner />
            Bridging…
          </span>
        ) : status.kind === "arrived" ? (
          <span className={STATUS}>
            <ThemedIcon className="size-4" src={`${BASE}/icon-check.svg`} />
            Arrived
          </span>
        ) : (
          <>
            <span className={STATUS}>Failed</span>
            <button
              aria-label={`Retry ${what}`}
              className={PILL}
              onClick={onConfirm}
              type="button"
            >
              Retry
            </button>
          </>
        )}
      </div>
      <span aria-live="polite" className="sr-only">
        {announcement(item, what)}
      </span>
    </div>
  );
}

function Enter({ children }: { children: ReactNode }) {
  return (
    <div className="t-incoming-enter">
      <div className="pt-2">{children}</div>
    </div>
  );
}

export function IncomingTransfers({
  incoming,
}: {
  incoming: IncomingTransfersState;
}) {
  const { items, error, confirm, reload } = incoming;
  if (items.length === 0 && !error) return null;
  return (
    <div className="t-incoming-enter">
      <div className="flex flex-col px-2 pb-2">
        {items.map((item) => (
          <Enter key={item.key}>
            <IncomingTransferCard
              item={item}
              onConfirm={() => void confirm(item)}
            />
          </Enter>
        ))}
        {error ? (
          <Enter>
            <div className="flex w-full items-center gap-3 rounded-[20px] bg-accent px-4 py-3">
              <p className="min-w-0 flex-1 text-[13px] text-muted-foreground leading-4" role="status">
                {error}
              </p>
              <button className={PILL} onClick={reload} type="button">
                Retry
              </button>
            </div>
          </Enter>
        ) : null}
      </div>
    </div>
  );
}
