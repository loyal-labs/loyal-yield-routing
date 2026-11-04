"use client";

import { type ReactNode, useRef, useState } from "react";

import {
  ActionErrorBody,
  ActionProcessingBody,
  ActionSuccessBody,
  formatUsdAmount,
} from "@/components/wallet-workspace/facelift/action-screens";
import { usePublicEnv } from "@/contexts/public-env-context";
import { openTrackedLink } from "@/lib/core/analytics";

import {
  BRIDGE_AMOUNT_TEXT,
  BridgeCta,
  BridgeDivider,
  BridgeIcon,
  BridgePickerHeader,
  BridgePickerHost,
  BridgePickerRow,
  BridgeQuoteRows,
  BridgeRecipientRow,
  BridgeScreen,
  TokenPill,
  useBridgeDesktop,
  useBridgePickerFocus,
} from "./bridge-pieces";
import { useEscapeOverlay, useEscapePage } from "./escape-owner";
import { PaneReveal } from "./pane-transitions";
import {
  fromRaw,
  type MoveSource,
  receivedAmount,
  toRaw,
  useBridgeMove,
} from "./use-bridge-move";

const PLACEHOLDER = "asset-placeholder.svg";
const relayExplorer = (requestId: string) =>
  `https://relay.link/transaction/${requestId}`;

const plain = (formatted: string) => Number(formatted.replace(/,/g, ""));

const usd = (value: number) =>
  value > 0 && value < 0.01 ? "<$0.01" : `$${formatUsdAmount(value)}`;

const eta = (seconds: number | null) =>
  seconds === null
    ? "—"
    : seconds < 90
    ? `~${Math.max(1, Math.round(seconds))} s`
    : `~${Math.round(seconds / 60)} min`;

function ExplorerButton({ href, source }: { href: string; source: string }) {
  const publicEnv = usePublicEnv();
  return (
    <button
      className="t-hover flex h-12 items-center justify-center rounded-full bg-accent px-5 font-medium text-[16px] text-foreground leading-5 hover:bg-accent-active"
      onClick={() =>
        openTrackedLink(publicEnv, {
          href,
          linkText: "View in Explorer",
          source,
        })
      }
      type="button"
    >
      View in Explorer
    </button>
  );
}

export type BridgeStatus =
  | { kind: "bridging"; icons: ReactNode; requestId: string }
  | { kind: "filled"; signature: string | null; explorerHref?: string }
  | { kind: "failed"; message: string; requestId: string | null };

export function BridgeStatusBody({
  status,
  source,
  onBack,
}: {
  status: BridgeStatus;
  source: string;
  onBack: () => void;
}) {
  if (status.kind === "bridging")
    return (
      <ActionProcessingBody icons={status.icons} label="Bridging...">
        <div className="flex flex-col items-center gap-3">
          <ExplorerButton
            href={relayExplorer(status.requestId)}
            source={`${source}_processing`}
          />
          <button
            className="t-hover flex h-12 items-center justify-center rounded-full bg-foreground px-5 font-medium text-[16px] text-background leading-5 hover:-translate-y-0.5 hover:bg-foreground/90 active:translate-y-0"
            onClick={onBack}
            type="button"
          >
            Done
          </button>
        </div>
      </ActionProcessingBody>
    );
  if (status.kind === "filled")
    return (
      <ActionSuccessBody
        explorerHref={status.explorerHref}
        label="Swapped"
        onDone={onBack}
        signature={status.signature}
        source={`${source}_success`}
      />
    );
  return (
    <ActionErrorBody
      backLabel="Done"
      explorer={
        status.requestId ? (
          <ExplorerButton
            href={relayExplorer(status.requestId)}
            source={`${source}_failed`}
          />
        ) : null
      }
      message={status.message}
      onBack={onBack}
    />
  );
}

export function BridgeMove({
  source,
  onBack,
}: {
  source: MoveSource;
  onBack: () => void;
}) {
  const desktop = useBridgeDesktop();
  const [balance, setBalance] = useState(source.start);
  const [picking, setPicking] = useState(false);
  const [editing, setEditing] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  const amountRef = useRef<HTMLInputElement>(null);
  const move = useBridgeMove(balance, source.sender);
  const { step, quote } = move;
  const closePicker = () => setPicking(false);
  useEscapeOverlay(picking, closePicker);
  const sending = step.kind === "sending";
  useEscapePage(!sending, onBack);
  useBridgePickerFocus(panelRef, picking, desktop);

  const hasAmount = move.amountRaw > BigInt(0);
  const rate = quote
    ? plain(receivedAmount(quote.amountOutRaw)) /
      plain(fromRaw(move.amountRaw, balance.decimals))
    : 0;
  const quoteError =
    hasAmount && !move.insufficient && !move.quoting ? move.quoteError : null;
  const cta: { tone: "ready" | "blocked" | "busy"; label: string } = !hasAmount
    ? { tone: "blocked", label: "Enter amount" }
    : move.insufficient
    ? { tone: "blocked", label: "Insufficient balance" }
    : quoteError
    ? { tone: "blocked", label: "Change amount" }
    : sending
    ? { tone: "busy", label: "Sending…" }
    : move.elsewhere
    ? { tone: "blocked", label: "Move in progress" }
    : !move.ready
    ? { tone: "busy", label: "Getting quote…" }
    : { tone: "ready", label: "Send now" };

  const form = (
    <>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="p-2 pb-4">
          <div className="flex flex-col gap-0.5 px-4 py-2">
            <label
              className="text-[16px] leading-5 text-muted-foreground"
              htmlFor="bridge-move-amount"
            >
              You send
            </label>
            <div className="flex items-center">
              <input
                aria-label="You send amount"
                className={`h-12 bg-transparent outline-none placeholder:text-tertiary ${BRIDGE_AMOUNT_TEXT}`}
                disabled={sending}
                id="bridge-move-amount"
                inputMode="decimal"
                onBlur={() => setEditing(false)}
                onChange={(event) => {
                  const value = event.target.value.replace(/,/g, "");
                  if (/^\d{0,12}(\.\d{0,6})?$/.test(value))
                    move.setAmountInput(value);
                }}
                onFocus={() => setEditing(true)}
                placeholder="0"
                ref={amountRef}
                value={
                  editing || !move.amountInput
                    ? move.amountInput
                    : fromRaw(
                        toRaw(move.amountInput, balance.decimals),
                        balance.decimals
                      )
                }
              />
              <TokenPill
                badge={balance.chainIcon ?? PLACEHOLDER}
                icon={balance.icon ?? PLACEHOLDER}
                label="Select asset to send"
                network={balance.chainName}
                onClick={sending ? undefined : () => setPicking(true)}
                symbol={balance.symbol}
              />
            </div>
            <div className="flex items-center justify-end text-[16px] leading-5">
              <button
                className="t-hover flex gap-1"
                disabled={sending}
                onClick={() =>
                  move.setAmountInput(
                    fromRaw(balance.amountRaw, balance.decimals).replace(
                      /,/g,
                      ""
                    )
                  )
                }
                type="button"
              >
                MAX{" "}
                <span className="text-muted-foreground">
                  {fromRaw(balance.amountRaw, balance.decimals)}
                </span>
              </button>
            </div>
          </div>
          <BridgeDivider />
          <div className="flex flex-col gap-0.5 px-4 py-2">
            <p className="text-[16px] leading-5 text-muted-foreground">
              You receive
            </p>
            <div className="flex items-center">
              <output
                aria-label="You receive amount"
                className={`truncate ${BRIDGE_AMOUNT_TEXT} ${
                  quote ? "" : "text-tertiary"
                }`}
              >
                {quote ? receivedAmount(quote.amountOutRaw) : "0"}
              </output>
              <TokenPill
                badge="sol.png"
                icon="usdc.png"
                label="You receive USDC on Solana"
                network="Solana"
                symbol="USDC"
              />
            </div>
          </div>
        </div>
        <BridgeRecipientRow network="Solana" recipient={source.recipient} />
        {quote && !move.insufficient && (
          <BridgeQuoteRows
            label="Bridge quote"
            rows={[
              [
                "Rate",
                `1 ${balance.symbol} ~ ${rate.toLocaleString("en-US", {
                  maximumSignificantDigits: 5,
                })} USDC`,
              ],
              [
                "Minimum received",
                `${receivedAmount(quote.amountOutMinRaw)} USDC`,
              ],
              ["Relay fee", usd(Number(quote.relayerFeeUsd))],
              ["Network fee", "Free"],
              ["Estimated time", eta(quote.etaSeconds)],
            ]}
          />
        )}
      </div>
      <footer className="shrink-0 px-4 pt-2 pb-4">
        {quoteError && (
          <p
            className="pb-3 text-center text-[13px] text-destructive"
            role="alert"
          >
            {quoteError}
          </p>
        )}
        <BridgeCta
          label={cta.label}
          onClick={() => {
            if (cta.tone === "ready") void move.send();
            else amountRef.current?.focus();
          }}
          tone={cta.tone}
        />
      </footer>
    </>
  );

  const screen =
    step.kind === "form" || step.kind === "sending" ? (
      form
    ) : (
      <BridgeStatusBody
        onBack={onBack}
        source="bridge_move"
        status={
          step.kind === "bridging"
            ? {
                kind: "bridging",
                icons: (
                  <>
                    <span className="-mr-4 flex">
                      <BridgeIcon file={balance.icon ?? PLACEHOLDER} size={64} />
                    </span>
                    <BridgeIcon file={balance.chainIcon ?? PLACEHOLDER} size={64} />
                  </>
                ),
                requestId: step.quote.requestId,
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
    );

  return (
    <>
      <BridgeScreen onBack={sending ? undefined : onBack}>
        <PaneReveal key={step.kind === "sending" ? "form" : step.kind}>
          {screen}
        </PaneReveal>
      </BridgeScreen>
      <BridgePickerHost
        desktop={desktop}
        onClose={closePicker}
        open={picking}
        title="You send"
        view="send"
      >
        <div
          aria-label="You send"
          className="flex h-full min-h-0 flex-col"
          ref={panelRef}
        >
          <BridgePickerHeader onClose={closePicker} title="You send" />
          <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
            {source.balances.map((item) => (
              <BridgePickerRow
                badge={item.chainIcon ?? PLACEHOLDER}
                detail={item.chainName}
                icon={item.icon ?? PLACEHOLDER}
                key={`${item.chain}:${item.asset}`}
                name={item.name}
                onClick={() => {
                  if (item.chain !== balance.chain || item.asset !== balance.asset) {
                    setBalance(item);
                    move.setAmountInput("");
                  }
                  closePicker();
                }}
                value={[fromRaw(item.amountRaw, item.decimals), item.symbol]}
              />
            ))}
          </div>
        </div>
      </BridgePickerHost>
    </>
  );
}
