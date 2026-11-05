"use client";

import { useState } from "react";

import {
  ScrambleText,
  useBalanceVisibility,
} from "@/components/wallet-workspace/facelift/balance-visibility";

import { BridgeStatusBody } from "./bridge-move";
import { BridgeIcon } from "./bridge-pieces";
import { ThemedIcon } from "./themed-icon";
import {
  fromRaw,
  type IncomingTransfer,
  useBridgeMove,
} from "./use-bridge-move";

const BASE = "/wallet-workspace/facelift";
const PLACEHOLDER = "asset-placeholder.svg";

function amountLabel({ amountRaw, decimals, symbol }: IncomingTransfer) {
  const [whole, fraction = ""] = fromRaw(amountRaw, decimals).split(".");
  return `+${whole}.${fraction.padEnd(2, "0")} ${symbol}`;
}

export function IncomingTransferCard({
  transfer,
  sender,
  onSettled,
}: {
  transfer: IncomingTransfer;
  sender: string;
  onSettled: () => void;
}) {
  const { isBalanceHidden } = useBalanceVisibility();
  const { step, confirm, reset } = useBridgeMove(transfer, sender, onSettled);
  const icon = transfer.icon ?? PLACEHOLDER;
  const underMinimum =
    transfer.amountRaw <
    BigInt(transfer.minUsd) * BigInt(10) ** BigInt(transfer.decimals);
  const sending = step.kind === "sending";
  const [hidden, setHidden] = useState(false);

  if (hidden) return null;

  if (step.kind === "form" || sending)
    return (
      <div className="flex w-full items-center rounded-[20px] bg-accent px-4">
        <div className="flex shrink-0 items-center py-2 pr-3">
          <span className="relative flex">
            <BridgeIcon file={icon} />
            <span className="-right-0.5 -bottom-0.5 absolute flex size-[18px] items-center justify-center rounded-full border-2 border-card bg-tertiary">
              <ThemedIcon
                className="size-2.5 text-white"
                src={`${BASE}/sidebar/receive.svg`}
              />
            </span>
          </span>
        </div>
        <div className="flex min-w-0 flex-1 flex-col gap-0.5 py-[11px]">
          <p className="truncate text-[13px] text-muted-foreground leading-4">
            {underMinimum
              ? `Under the $${transfer.minUsd} minimum`
              : "Confirm incoming transfer"}
          </p>
          <p className="truncate font-medium text-[16px] text-foreground leading-5 tracking-[-0.176px]">
            <ScrambleText
              isHidden={isBalanceHidden}
              text={amountLabel(transfer)}
            />{" "}
            <span className="text-tertiary">from {transfer.shortName}</span>
          </p>
        </div>
        <button
          className="ml-3 flex min-w-16 shrink-0 items-center justify-center rounded-[40px] bg-foreground px-4 py-2.5 font-medium text-[13px] text-background leading-4 transition-colors enabled:hover:bg-foreground/90 disabled:opacity-40"
          disabled={underMinimum || sending}
          onClick={() => void confirm()}
          type="button"
        >
          {sending ? "Sending…" : "Confirm"}
        </button>
      </div>
    );

  return (
    <div
      className={`relative flex w-full flex-col overflow-clip rounded-[20px] bg-accent ${
        step.kind === "bridging" ? "" : "min-h-[800px]"
      }`}
    >
      <BridgeStatusBody
        onBack={
          step.kind === "bridging"
            ? () => setHidden(true)
            : step.kind === "failed"
            ? () => {
                reset();
                onSettled();
              }
            : onSettled
        }
        source="incoming_transfer"
        status={
          step.kind === "bridging"
            ? {
                kind: "bridging",
                icons: (
                  <>
                    <span className="-mr-4 flex">
                      <BridgeIcon file={icon} size={64} />
                    </span>
                    <BridgeIcon
                      file={transfer.chainIcon ?? PLACEHOLDER}
                      size={64}
                    />
                  </>
                ),
                requestId: step.requestId,
              }
            : step.kind === "filled"
            ? { kind: "filled", signature: step.transfer.destinationTx }
            : {
                kind: "failed",
                message: step.message,
                requestId: step.requestId,
              }
        }
      />
    </div>
  );
}
