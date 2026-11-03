"use client";

import { useRef, useState } from "react";

import {
  BRIDGE_AMOUNT_TEXT,
  BridgeCta,
  BridgeDivider,
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
import {
  BRIDGE_ASSETS,
  CHAINS,
  SOLANA_ASSETS,
  type BridgeAsset,
} from "./bridge-preview-data";
import { BridgeRecipientPane } from "./bridge-recipient-pane";
import { useEscapeOverlay, useEscapePage } from "./escape-owner";
import { PaneReveal } from "./pane-transitions";
import { ThemedIcon } from "./themed-icon";

const BASE = "/wallet-workspace/facelift";
const SEND_ASSETS = [SOLANA_ASSETS[0], BRIDGE_ASSETS[0], SOLANA_ASSETS[1]];
type Picker = "send" | "receive" | "chain" | "recipient";
type Chain = (typeof CHAINS)[number];

function formatAmount(value: number, decimals = 2) {
  return value.toLocaleString("en-US", { maximumFractionDigits: decimals });
}

function dollars(value: number) {
  return value.toLocaleString("en-US", { style: "currency", currency: "USD" });
}

export function BridgeWorkspace({ onBack }: { onBack: () => void }) {
  const desktop = useBridgeDesktop();
  const [sendAsset, setSendAsset] = useState<BridgeAsset>(SEND_ASSETS[0]);
  const [receiveAsset, setReceiveAsset] = useState<BridgeAsset>(
    BRIDGE_ASSETS[0]
  );
  const [{ chain, recipient }, setDestination] = useState<{
    chain: Chain;
    recipient: string;
  }>({ chain: CHAINS[0], recipient: "" });
  const [amount, setAmount] = useState("12000");
  const [editingAmount, setEditingAmount] = useState(false);
  const [inDollars, setInDollars] = useState(false);
  const [picker, setPicker] = useState<Picker | null>(null);
  const [search, setSearch] = useState("");
  const [previewMessage, setPreviewMessage] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLElement | null>(null);
  const amountRef = useRef<HTMLInputElement>(null);
  const lastPicker = useRef<Picker>("receive");
  if (picker) lastPicker.current = picker;
  const view = picker ?? lastPicker.current;
  const title =
    view === "send"
      ? "You send"
      : view === "receive"
      ? "You receive"
      : view === "chain"
      ? "Select chain"
      : `${chain.network} Recipient`;

  // shortcut: Figma balances and quotes only; replace all fixtures with backend data before launch.
  const price = sendAsset.symbol === "SOL" ? 150 : 0.999295;
  const entered = Number(amount) || 0;
  const tokenAmount = inDollars ? entered / price : entered;
  const received =
    tokenAmount * (sendAsset.symbol === "SOL" ? 150 : 1) * 0.99875;
  const max = sendAsset.symbol === "SOL" ? 0 : 6266.86;
  const hasAmount = tokenAmount > 0;

  const openPicker = (next: Picker) => {
    if (!picker)
      triggerRef.current =
        document.activeElement instanceof HTMLElement
          ? document.activeElement
          : null;
    setSearch("");
    setPreviewMessage(false);
    setPicker(next);
  };
  const closePicker = () => {
    setPicker(null);
    triggerRef.current?.focus({ preventScroll: true });
  };
  useEscapeOverlay(picker !== null, () =>
    view === "chain" ? openPicker("receive") : closePicker()
  );
  useEscapePage(true, onBack);
  useBridgePickerFocus(panelRef, picker !== null, desktop);

  const items =
    view === "chain" ? CHAINS : view === "send" ? SEND_ASSETS : BRIDGE_ASSETS;
  const filtered = items.filter((item) =>
    `${item.name} ${"symbol" in item ? item.symbol : item.network}`
      .toLowerCase()
      .includes(search.trim().toLowerCase())
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
          key={chain.id}
          isTron={chain.id === "tron"}
          network={chain.network}
          onSelect={(address) => {
            setDestination((current) =>
              current.chain.id === chain.id
                ? { ...current, recipient: address }
                : current
            );
            closePicker();
          }}
        />
      ) : (
        <>
          {view !== "send" && (
            <div className="px-4 pb-4">
              <label className="flex h-11 items-center gap-3 rounded-full bg-accent px-3">
                <ThemedIcon
                  className="size-6 shrink-0 text-muted-foreground"
                  src={`${BASE}/receive/search.svg`}
                />
                <input
                  aria-label={
                    view === "chain" ? "Search chains" : "Search assets"
                  }
                  className="min-w-0 flex-1 bg-transparent text-[16px] leading-5 outline-none placeholder:text-muted-foreground"
                  onChange={(event) => setSearch(event.target.value)}
                  placeholder={
                    view === "chain" ? "Search chains" : "Search assets"
                  }
                  type="search"
                  value={search}
                />
              </label>
            </div>
          )}
          <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
            {filtered.map((item) => (
              <BridgePickerRow
                badge={view === "send" ? "sol.png" : undefined}
                detail={"symbol" in item ? item.symbol : undefined}
                icon={"icon" in item ? item.icon : `${item.id}.png`}
                key={item.name}
                name={item.name}
                onClick={() => {
                  if ("id" in item) {
                    setDestination((current) =>
                      current.chain.id === item.id
                        ? current
                        : { chain: item, recipient: "" }
                    );
                    closePicker();
                  } else if (view === "send") {
                    setSendAsset(item);
                    setAmount(item.symbol === "SOL" ? "" : "12000");
                    setInDollars(false);
                    closePicker();
                  } else {
                    setReceiveAsset(item);
                    openPicker("chain");
                  }
                }}
                value={
                  view === "send" && "symbol" in item
                    ? [
                        item.symbol === "SOL" ? "$0.00" : "$9,750.00",
                        `${item.symbol === "SOL" ? "0.00" : "9,750.00"} ${
                          item.symbol
                        }`,
                      ]
                    : undefined
                }
              />
            ))}
            {filtered.length === 0 && (
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

  return (
    <>
      <BridgeScreen onBack={onBack}>
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
                    id="bridge-send-amount"
                    inputMode="decimal"
                    onBlur={() => setEditingAmount(false)}
                    onFocus={() => setEditingAmount(true)}
                    onChange={(event) => {
                      const value = event.target.value
                        .replace(/,/g, "")
                        .replace(/^\$/, "");
                      if (/^\d{0,12}(\.\d{0,6})?$/.test(value)) {
                        setAmount(value);
                        setPreviewMessage(false);
                      }
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
                    icon={sendAsset.icon}
                    label="Select asset to send"
                    network="Solana"
                    onClick={() => openPicker("send")}
                    symbol={sendAsset.symbol}
                  />
                </div>
                <div className="flex items-center justify-between text-[16px] leading-5">
                  <button
                    aria-label="Switch amount currency"
                    className="t-hover flex items-center gap-1 text-muted-foreground"
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
                      ? `${formatAmount(tokenAmount, 6)} ${sendAsset.symbol}`
                      : dollars(tokenAmount * price)}
                    <ThemedIcon
                      className="size-5"
                      src={`${BASE}/icon-arrow-rotate.svg`}
                    />
                  </button>
                  <button
                    className="t-hover flex gap-1"
                    onClick={() => {
                      setAmount(
                        String(
                          inDollars ? Number((max * price).toFixed(2)) : max
                        )
                      );
                      setPreviewMessage(false);
                    }}
                    type="button"
                  >
                    MAX{" "}
                    <span className="text-muted-foreground">
                      {formatAmount(max)}
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
                    className={`truncate ${BRIDGE_AMOUNT_TEXT}`}
                  >
                    {formatAmount(received)}
                  </output>
                  <TokenPill
                    badge={`${chain.id}.png`}
                    icon={receiveAsset.icon}
                    label="Select asset to receive"
                    network={chain.network}
                    onClick={() => openPicker("receive")}
                    symbol={receiveAsset.symbol}
                  />
                </div>
                <p className="text-[16px] leading-5 text-muted-foreground">
                  ~{dollars(received * (11971.21 / 11985))}
                </p>
              </div>
            </div>
            <BridgeRecipientRow
              active={picker === "recipient"}
              network={chain.network}
              onClear={() => {
                setDestination((current) => ({
                  ...current,
                  recipient: "",
                }));
                setPreviewMessage(false);
              }}
              onOpen={() => openPicker("recipient")}
              recipient={recipient}
            />
            {recipient && hasAmount && (
              <BridgeQuoteRows
                label="Bridge quote preview"
                rows={[
                  [
                    "Rate",
                    `1 ${sendAsset.symbol} ~ ${
                      sendAsset.symbol === "SOL" ? "148.8165" : "0.99211"
                    } ${receiveAsset.symbol}`,
                  ],
                  [
                    "Minimum received",
                    `~${dollars(received * (11962.21 / 11985))}`,
                  ],
                  ["Max slippage", "1%"],
                  ["Loyal fee", "0.4%"],
                  ["Network fee", "<$0.01"],
                ]}
              />
            )}
          </div>
          <footer className="shrink-0 px-4 pt-2 pb-4">
            {previewMessage && (
              <p
                className="pb-3 text-center text-[13px] text-muted-foreground"
                role="status"
              >
                Bridge preview — transfers are not available yet.
              </p>
            )}
            <BridgeCta
              label={
                !hasAmount
                  ? "Enter amount"
                  : !recipient
                  ? "Enter recipient"
                  : "Send now"
              }
              onClick={() => {
                if (!hasAmount) amountRef.current?.focus();
                else if (!recipient) openPicker("recipient");
                else setPreviewMessage(true);
              }}
              tone={recipient && hasAmount ? "ready" : "blocked"}
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
