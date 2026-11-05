"use client";

import { useRef, useState } from "react";

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
import { BridgeStatusBody } from "./bridge-move";
import { BridgeRecipientPane } from "./bridge-recipient-pane";
import { useEscapeOverlay, useEscapePage } from "./escape-owner";
import { PaneReveal } from "./pane-transitions";
import { ThemedIcon } from "./themed-icon";
import {
  type OutToken,
  useBridgeDestinations,
  useBridgeOut,
  useBridgeRecipients,
} from "./use-bridge-out";
import { fromRaw, toRaw } from "./use-bridge-move";

const BASE = "/wallet-workspace/facelift";
const PLACEHOLDER = "asset-placeholder.svg";
const ASSET_ORDER = new Map(
  ["usdt", "usdg", "pyusd", "usdc", "usds"].map((asset, index) => [asset, index])
);
const CHAIN_ORDER = new Map(
  ["ethereum", "tron", "bsc", "base", "arbitrum", "polygon", "avalanche"].map(
    (chain, index) => [chain, index]
  )
);
const USDC_DECIMALS = 6;
type Picker = "send" | "receive" | "chain" | "recipient";

function formatAmount(value: number, decimals = 2) {
  return value.toLocaleString("en-US", { maximumFractionDigits: decimals });
}

function dollars(value: number) {
  return value.toLocaleString("en-US", { style: "currency", currency: "USD" });
}

const networkLabel = (name: string) => name.replace(/\s*\(.*\)$/, "");

export function BridgeWorkspace({
  onBack,
  balance,
  price,
}: {
  onBack: () => void;
  balance: number;
  price: number;
}) {
  const desktop = useBridgeDesktop();
  const { destinations, error: destinationsError, retry } =
    useBridgeDestinations();
  const balanceRaw = toRaw(balance.toFixed(USDC_DECIMALS), USDC_DECIMALS);
  const [selection, setSelection] = useState<{
    chain: string | null;
    asset: string | null;
  }>({ chain: null, asset: null });
  const [recipient, setRecipient] = useState("");
  const [amount, setAmount] = useState("");
  const [editingAmount, setEditingAmount] = useState(false);
  const [inDollars, setInDollars] = useState(false);
  const [picker, setPicker] = useState<Picker | null>(null);
  const [pendingAsset, setPendingAsset] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const panelRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLElement | null>(null);
  const amountRef = useRef<HTMLInputElement>(null);
  const lastPicker = useRef<Picker>("receive");
  if (picker) lastPicker.current = picker;
  const view = picker ?? lastPicker.current;

  const popular = [...(destinations ?? [])]
    .filter((item) => item.popularRank > 0)
    .sort((a, b) => a.popularRank - b.popularRank);
  const destination =
    destinations?.find((item) => item.chain === selection.chain) ??
    popular[0] ??
    destinations?.[0] ??
    null;
  const tokens: OutToken[] = destination?.tokens ?? [];
  const token =
    tokens.find((item) => item.asset === selection.asset) ??
    tokens.find((item) => item.symbol === "USDT") ??
    tokens[0] ??
    null;
  const network = destination ? networkLabel(destination.name) : "";
  const chainId = destination?.chain ?? "";

  const entered = Number(amount) || 0;
  const tokenAmount = inDollars
    ? price > 0
      ? entered / price
      : 0
    : entered;
  const hasAmount = tokenAmount > 0;
  const out = useBridgeOut({
    chain: chainId,
    token,
    recipient,
    amountInput: hasAmount ? String(Number(tokenAmount.toFixed(6))) : "",
    balanceRaw,
  });
  const { quote, step } = out;
  const recents = useBridgeRecipients(chainId, picker === "recipient");

  const title =
    view === "send"
      ? "You send"
      : view === "receive"
      ? "You receive"
      : view === "chain"
      ? "Select chain"
      : `${network} Recipient`;

  const openPicker = (next: Picker) => {
    if (!picker)
      triggerRef.current =
        document.activeElement instanceof HTMLElement
          ? document.activeElement
          : null;
    setSearch("");
    if (next === "receive") setPendingAsset(null);
    setPicker(next);
  };
  const closePicker = () => {
    setPicker(null);
    triggerRef.current?.focus({ preventScroll: true });
  };
  const sending = step.kind === "sending";
  useEscapeOverlay(picker !== null, () =>
    view === "chain" ? openPicker("receive") : closePicker()
  );
  useEscapePage(!sending, onBack);
  useBridgePickerFocus(panelRef, picker !== null, desktop, view);

  const pickChain = (chain: string) => {
    if (!pendingAsset) return;
    if (chain !== chainId) setRecipient("");
    setSelection({ chain, asset: pendingAsset });
    closePicker();
  };

  const query = search.trim().toLowerCase();
  const assetMatches = [
    ...new Map(
      (destinations ?? []).flatMap((item) =>
        (item.tokens ?? []).map((asset) => [asset.asset, asset] as const)
      )
    ).values(),
  ]
    .filter((item) => `${item.name} ${item.symbol}`.toLowerCase().includes(query))
    .sort((a, b) =>
      (ASSET_ORDER.get(a.asset) ?? ASSET_ORDER.size) -
      (ASSET_ORDER.get(b.asset) ?? ASSET_ORDER.size)
    );
  const chainMatches = (destinations ?? [])
    .filter((item) =>
      item.tokens?.some((asset) => asset.asset === pendingAsset) &&
      `${item.name} ${item.chain}`.toLowerCase().includes(query)
    )
    .sort((a, b) =>
      (CHAIN_ORDER.get(a.chain) ?? CHAIN_ORDER.size) -
      (CHAIN_ORDER.get(b.chain) ?? CHAIN_ORDER.size)
    );

  const pickerContent = (
    <div
      className="flex h-full min-h-0 flex-col"
      ref={panelRef}
      aria-label={title}
    >
      <BridgePickerHeader
        onBack={view === "chain" ? () => openPicker("receive") : undefined}
        onClose={closePicker}
        title={title}
      />
      {view === "recipient" ? (
        <BridgeRecipientPane
          key={chainId}
          isTron={destination?.addressKind === "tron"}
          network={network}
          onSelect={(address) => {
            setRecipient(address);
            closePicker();
          }}
          recents={recents}
        />
      ) : view === "send" ? (
        <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
          <BridgePickerRow
            badge="sol.png"
            detail="USDC"
            icon="usdc.png"
            name="USD Coin"
            onClick={closePicker}
            value={[
              dollars(balance * price),
              `${formatAmount(balance)} USDC`,
            ]}
          />
        </div>
      ) : (
        <>
          <div className="px-4 pb-4">
            <label className="flex h-11 items-center gap-3 rounded-full bg-accent px-3">
              <ThemedIcon
                className="size-6 shrink-0 text-muted-foreground"
                src={`${BASE}/receive/search.svg`}
              />
              <input
                aria-label={view === "chain" ? "Search chains" : "Search assets"}
                className="min-w-0 flex-1 bg-transparent text-[16px] leading-5 outline-none placeholder:text-muted-foreground"
                onChange={(event) => setSearch(event.target.value)}
                placeholder={view === "chain" ? "Search chains" : "Search assets"}
                type="search"
                value={search}
              />
            </label>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
            {view === "chain"
              ? chainMatches.map((item) => {
                  const minimum = item.tokens?.find(
                    (asset) => asset.asset === pendingAsset
                  )?.minUsdc ?? 0;
                  return (
                    <BridgePickerRow
                      detail={minimum > 0 ? `Minimum ${minimum} USDC` : undefined}
                      icon={item.icon || PLACEHOLDER}
                      key={item.chain}
                      name={item.name}
                      onClick={() => pickChain(item.chain)}
                    />
                  );
                })
              : assetMatches.map((item) => (
                  <BridgePickerRow
                    detail={item.symbol}
                    icon={item.icon || PLACEHOLDER}
                    key={item.asset}
                    name={item.name}
                    onClick={() => {
                      setPendingAsset(item.asset);
                      openPicker("chain");
                    }}
                  />
                ))}
            {(view === "chain" ? chainMatches : assetMatches).length === 0 && (
              <p
                className="px-4 py-6 text-[16px] text-muted-foreground"
                role="status"
              >
                No {view === "chain" ? "chains" : "assets"} found
              </p>
            )}
          </div>
        </>
      )}
    </div>
  );

  if (!destination || !token)
    return (
      <BridgeScreen onBack={onBack}>
        <PaneReveal>
          <div className="flex flex-col items-start gap-3 px-6 py-6 text-[16px] leading-5 text-muted-foreground">
            <p role="status">
              {destinationsError ?? "Loading chains..."}
            </p>
            {destinationsError && (
              <button
                className="t-hover rounded-full bg-accent px-4 py-2.5 text-[13px] font-medium leading-4 text-foreground hover:bg-accent-active"
                onClick={retry}
                type="button"
              >
                Try again
              </button>
            )}
          </div>
        </PaneReveal>
      </BridgeScreen>
    );

  if (step.kind === "bridging" || step.kind === "filled" || step.kind === "failed")
    return (
      <BridgeScreen onBack={onBack}>
        <PaneReveal key={step.kind}>
          <BridgeStatusBody
            onBack={onBack}
            source="bridge_out"
            status={
              step.kind === "bridging"
                ? {
                    kind: "bridging",
                    icons: (
                      <>
                        <span className="-mr-4 flex">
                          <BridgeIcon file="usdc.png" size={64} />
                        </span>
                        <BridgeIcon
                          file={destination.icon || PLACEHOLDER}
                          size={64}
                        />
                      </>
                    ),
                    requestId: step.requestId,
                  }
                : step.kind === "filled"
                ? {
                    kind: "filled",
                    signature: step.transfer.destinationTx,
                    explorerHref:
                      step.transfer.destinationTx &&
                      destinations?.find(
                        (item) => item.chain === step.transfer.chain
                      )
                        ? `${
                            destinations.find(
                              (item) => item.chain === step.transfer.chain
                            )?.explorerTx
                          }${step.transfer.destinationTx}`
                        : undefined,
                  }
                : {
                    kind: "failed",
                    message: step.message,
                    requestId: step.requestId,
                  }
            }
          />
        </PaneReveal>
      </BridgeScreen>
    );

  const minimum = token.minUsdc;
  const recipientRejected = out.quoteError?.code === "invalid_recipient";
  const formError = recipientRejected
    ? `Invalid ${network} address`
    : out.belowMinimum
    ? `The smallest amount to send there is ${minimum} USDC.`
    : out.quoteError?.message ?? null;
  const cta: { tone: "ready" | "blocked" | "busy"; label: string } = !hasAmount
    ? { tone: "blocked", label: "Enter amount" }
    : out.insufficient
    ? { tone: "blocked", label: "Insufficient balance" }
    : !recipient
    ? { tone: "blocked", label: "Enter recipient" }
    : out.quoteError?.retryable
    ? { tone: "ready", label: "Try again" }
    : out.belowMinimum || out.quoteError
    ? {
        tone: "blocked",
        label: recipientRejected ? "Enter recipient" : "Change amount",
      }
    : sending
    ? { tone: "busy", label: "Sending…" }
    : out.elsewhere
    ? { tone: "blocked", label: "Transfer in progress" }
    : !out.ready
    ? { tone: "busy", label: "Getting quote…" }
    : { tone: "ready", label: "Send now" };
  const received = quote ? fromRaw(quote.amountOutRaw, token.decimals) : "0";
  const asRow = (value: string, format: (value: string) => string) =>
    value ? format(value) : "—";

  return (
    <>
      <BridgeScreen onBack={sending ? undefined : onBack}>
        <PaneReveal>
          <div className="min-h-0 flex-1 overflow-y-auto">
            <div className="p-2 pb-4">
              <div className="flex flex-col gap-0.5 px-4 py-2">
                <label
                  className="text-[16px] leading-5 text-muted-foreground"
                  htmlFor="bridge-send-amount"
                >
                  You send
                </label>
                <div className="flex items-center">
                  <input
                    aria-label="You send amount"
                    className={`h-12 bg-transparent outline-none placeholder:text-tertiary ${BRIDGE_AMOUNT_TEXT}`}
                    disabled={sending}
                    id="bridge-send-amount"
                    inputMode="decimal"
                    onBlur={() => setEditingAmount(false)}
                    onFocus={() => setEditingAmount(true)}
                    onChange={(event) => {
                      const value = event.target.value
                        .replace(/,/g, "")
                        .replace(/^\$/, "");
                      if (/^\d{0,12}(\.\d{0,6})?$/.test(value))
                        setAmount(value);
                    }}
                    placeholder="0"
                    ref={amountRef}
                    value={
                      editingAmount
                        ? amount
                        : amount
                        ? `${inDollars ? "$" : ""}${formatAmount(entered, 6)}`
                        : ""
                    }
                  />
                  <TokenPill
                    badge="sol.png"
                    icon="usdc.png"
                    label="Select asset to send"
                    network="Solana"
                    onClick={sending ? undefined : () => openPicker("send")}
                    symbol="USDC"
                  />
                </div>
                <div className="flex items-center justify-between text-[16px] leading-5">
                  <button
                    aria-label="Switch amount currency"
                    className="t-hover flex items-center gap-1 text-muted-foreground"
                    disabled={sending}
                    onClick={() => {
                      setAmount(
                        String(
                          Number(
                            (inDollars
                              ? tokenAmount
                              : tokenAmount * price
                            ).toFixed(6)
                          )
                        )
                      );
                      setInDollars(!inDollars);
                    }}
                    type="button"
                  >
                    {inDollars
                      ? `${formatAmount(tokenAmount, 6)} USDC`
                      : dollars(tokenAmount * price)}
                    <ThemedIcon
                      className="size-5"
                      src={`${BASE}/icon-arrow-rotate.svg`}
                    />
                  </button>
                  <button
                    className="t-hover flex gap-1"
                    disabled={sending}
                    onClick={() =>
                      setAmount(
                        String(
                          inDollars
                            ? Math.floor(balance * price * 100) / 100
                            : balance
                        )
                      )
                    }
                    type="button"
                  >
                    MAX{" "}
                    <span className="text-muted-foreground">
                      {formatAmount(balance)}
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
                    {received}
                  </output>
                  <TokenPill
                    badge={destination.icon || PLACEHOLDER}
                    icon={token.icon || PLACEHOLDER}
                    label="Select asset to receive"
                    network={network}
                    onClick={sending ? undefined : () => openPicker("receive")}
                    symbol={token.symbol}
                  />
                </div>
                <p className="text-[16px] leading-5 text-muted-foreground">
                  {quote
                    ? quote.amountOutUsd
                      ? `~${dollars(Number(quote.amountOutUsd))}`
                      : " "
                    : hasAmount
                    ? " "
                    : "$0.00"}
                </p>
              </div>
            </div>
            <BridgeRecipientRow
              active={picker === "recipient"}
              network={network}
              onClear={sending ? undefined : () => setRecipient("")}
              onOpen={sending ? undefined : () => openPicker("recipient")}
              recipient={recipient}
            />
            {recipient && hasAmount && quote && !out.insufficient && (
              <BridgeQuoteRows
                label="Bridge quote"
                rows={[
                  [
                    "Rate",
                    asRow(quote.rate, (rate) => `1 USDC ~ ${rate} ${token.symbol}`),
                  ],
                  [
                    "Minimum received",
                    `${fromRaw(quote.amountOutMinRaw, token.decimals)} ${
                      token.symbol
                    }`,
                  ],
                  ["Max slippage", `${quote.slippageBps / 100}%`],
                  ["Loyal fee", `${quote.loyalFeeBps / 100}%`],
                  [
                    "Network fee",
                    asRow(quote.networkFeeUsd, (fee) =>
                      Number(fee) < 0.01 ? "<$0.01" : dollars(Number(fee))
                    ),
                  ],
                ]}
              />
            )}
          </div>
          <footer className="shrink-0 px-4 pt-2 pb-4">
            {(formError || out.notice) && (
              <p
                className={`pb-3 text-center text-[13px] ${
                  formError ? "text-destructive" : "text-muted-foreground"
                }`}
                role={formError ? "alert" : "status"}
              >
                {formError ?? out.notice}
              </p>
            )}
            <BridgeCta
              label={cta.label}
              onClick={() => {
                if (out.quoteError?.retryable) void out.retry();
                else if (cta.tone === "ready") void out.send();
                else if (!hasAmount || out.insufficient || out.belowMinimum)
                  amountRef.current?.focus();
                else if (!recipient || recipientRejected)
                  openPicker("recipient");
                else amountRef.current?.focus();
              }}
              tone={cta.tone}
            />
          </footer>
        </PaneReveal>
      </BridgeScreen>
      <BridgePickerHost
        desktop={desktop}
        onClose={closePicker}
        open={picker !== null}
        title={title}
        view={view}
      >
        {pickerContent}
      </BridgePickerHost>
    </>
  );
}
