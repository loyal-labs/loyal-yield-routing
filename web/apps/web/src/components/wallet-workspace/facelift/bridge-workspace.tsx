"use client";

import { useEffect, useRef, useState, useSyncExternalStore } from "react";

import {
  BRIDGE_ASSETS,
  CHAINS,
  SOLANA_ASSETS,
  type BridgeAsset,
} from "./bridge-preview-data";
import {
  BridgeRecipientPane,
  BridgeWalletIcon,
  shortBridgeAddress,
} from "./bridge-recipient-pane";
import { useEscapeOverlay, useEscapePage } from "./escape-owner";
import { PaneReveal } from "./pane-transitions";
import { InlineSheetReveal, SheetReveal } from "./sheet-reveal";
import { TextSwap } from "./text-swap";
import { TokenIconSwap } from "./token-icon-swap";
import { ThemedIcon } from "./themed-icon";

const BASE = "/wallet-workspace/facelift";
const DESKTOP_QUERY = "(min-width: 1204px)";
const SEND_ASSETS = [SOLANA_ASSETS[0], BRIDGE_ASSETS[0], SOLANA_ASSETS[1]];
type Picker = "send" | "receive" | "chain" | "recipient";
type Chain = (typeof CHAINS)[number];

function subscribeDesktop(onChange: () => void) {
  const media = window.matchMedia(DESKTOP_QUERY);
  media.addEventListener("change", onChange);
  return () => media.removeEventListener("change", onChange);
}

function formatAmount(value: number, decimals = 2) {
  return value.toLocaleString("en-US", { maximumFractionDigits: decimals });
}

function dollars(value: number) {
  return value.toLocaleString("en-US", { style: "currency", currency: "USD" });
}

function BridgeIcon({
  file,
  chain,
  size = 44,
}: {
  file: string;
  chain?: string;
  size?: number;
}) {
  return (
    <span className="relative shrink-0" style={{ width: size, height: size }}>
      {size === 36 ? (
        <TokenIconSwap src={`${BASE}/receive/${file}`} />
      ) : (
        // eslint-disable-next-line @next/next/no-img-element
        <img
          alt=""
          className="size-full rounded-full object-cover"
          height={size}
          src={`${BASE}/receive/${file}`}
          width={size}
        />
      )}
      {chain && (
        // eslint-disable-next-line @next/next/no-img-element
        <img
          alt=""
          className="absolute right-0 bottom-0 size-4 rounded-full border-2 border-card"
          height={16}
          src={`${BASE}/receive/${chain}.png`}
          width={16}
        />
      )}
    </span>
  );
}

function TokenPill({
  asset,
  chain,
  network,
  label,
  onClick,
}: {
  asset: BridgeAsset;
  chain: string;
  network: string;
  label: string;
  onClick: () => void;
}) {
  return (
    <button
      aria-label={label}
      className="t-hover ml-3 flex shrink-0 items-center rounded-full bg-accent py-1 pr-2.5 pl-1 text-left hover:bg-accent-active"
      onClick={onClick}
      type="button"
    >
      <BridgeIcon chain={chain} file={asset.icon} size={36} />
      <span className="flex flex-col pl-2">
        <span className="text-[16px] font-medium leading-5">
          <TextSwap text={asset.symbol} />
        </span>
        <span className="text-[13px] leading-4 text-muted-foreground">
          <TextSwap text={network} />
        </span>
      </span>
      <ThemedIcon
        className="size-6 text-tertiary"
        src={`${BASE}/icon-chevron-right.svg`}
      />
    </button>
  );
}

export function BridgeWorkspace({ onBack }: { onBack: () => void }) {
  const desktop = useSyncExternalStore(
    subscribeDesktop,
    () => window.matchMedia(DESKTOP_QUERY).matches,
    () => true
  );
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

  useEffect(() => {
    if (!picker) return;
    const panel = panelRef.current;
    const frame = requestAnimationFrame(() =>
      (
        panel?.querySelector<HTMLElement>("input, textarea") ??
        panel?.querySelector<HTMLElement>("button")
      )?.focus({ preventScroll: true })
    );
    const trapFocus = (event: KeyboardEvent) => {
      if (desktop || event.key !== "Tab" || !panel) return;
      const nodes = Array.from(
        panel.querySelectorAll<HTMLElement>(
          "button, input, textarea, [tabindex='0']"
        )
      ).filter((node) => !node.hasAttribute("disabled"));
      const first = nodes[0],
        last = nodes.at(-1);
      if (
        event.shiftKey &&
        (document.activeElement === first ||
          !panel.contains(document.activeElement))
      ) {
        event.preventDefault();
        last?.focus();
      } else if (
        !event.shiftKey &&
        (document.activeElement === last ||
          !panel.contains(document.activeElement))
      ) {
        event.preventDefault();
        first?.focus();
      }
    };
    window.addEventListener("keydown", trapFocus, true);
    return () => {
      cancelAnimationFrame(frame);
      window.removeEventListener("keydown", trapFocus, true);
    };
  }, [picker, desktop]);

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
      <header className="flex shrink-0 items-center p-2">
        {view === "chain" && (
          <button
            aria-label="Back to receive assets"
            className="t-hover flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
            onClick={() => openPicker("receive")}
            type="button"
          >
            <ThemedIcon
              className="size-6 text-muted-foreground"
              src={`${BASE}/icon-arrow-left.svg`}
            />
          </button>
        )}
        <h2
          className={`min-w-0 flex-1 truncate py-2.5 text-[20px] font-semibold leading-6 ${
            view === "chain" ? "pl-3" : "pl-4"
          }`}
        >
          {title}
        </h2>
        <button
          aria-label="Close bridge selector"
          className="t-hover flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
          onClick={closePicker}
          type="button"
        >
          <ThemedIcon
            className="size-6 text-muted-foreground"
            src={`${BASE}/icon-cross.svg`}
          />
        </button>
      </header>
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
              <button
                className="t-hover flex min-h-[60px] w-full items-center gap-3 rounded-2xl px-4 text-left hover:bg-accent focus-visible:bg-accent"
                key={item.name}
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
                type="button"
              >
                <BridgeIcon
                  chain={view === "send" ? "sol" : undefined}
                  file={"icon" in item ? item.icon : `${item.id}.png`}
                />
                <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                  <span className="text-[16px] font-medium leading-5">
                    {item.name}
                  </span>
                  {"symbol" in item && (
                    <span className="text-[13px] leading-4 text-muted-foreground">
                      {item.symbol}
                    </span>
                  )}
                </span>
                {view === "send" && "symbol" in item && (
                  <span className="flex flex-col items-end gap-0.5">
                    <span className="text-[16px] font-medium leading-5">
                      {item.symbol === "SOL" ? "$0.00" : "$9,750.00"}
                    </span>
                    <span className="text-[13px] leading-4 text-muted-foreground">
                      {item.symbol === "SOL" ? "0.00" : "9,750.00"}{" "}
                      {item.symbol}
                    </span>
                  </span>
                )}
              </button>
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
      <section
        data-bridge-preview
        aria-label="Bridge"
        className="flex h-full min-w-0 flex-1 flex-col overflow-clip rounded-3xl bg-card"
      >
        <header className="flex shrink-0 items-center p-2">
          <button
            aria-label="Back to assets"
            className="t-hover flex size-11 items-center justify-center rounded-full hover:bg-accent min-[796px]:hidden"
            onClick={onBack}
            type="button"
          >
            <ThemedIcon
              className="size-6"
              src={`${BASE}/icon-arrow-left.svg`}
            />
          </button>
          <h1 className="px-4 py-2.5 text-[20px] font-semibold leading-6">
            Bridge
          </h1>
        </header>
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
                    className="h-12 min-w-0 flex-1 bg-transparent text-[40px] font-semibold leading-[48px] tracking-[-1px] outline-none placeholder:text-tertiary max-[480px]:text-[32px]"
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
                    asset={sendAsset}
                    chain="sol"
                    label="Select asset to send"
                    network="Solana"
                    onClick={() => openPicker("send")}
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
              <div
                aria-hidden="true"
                className="relative mx-4 flex h-11 items-center justify-center"
              >
                <div className="absolute inset-x-0 top-1/2 border-t border-border" />
                <span className="relative flex size-11 items-center justify-center rounded-full bg-accent backdrop-blur-sm">
                  <ThemedIcon
                    className="size-6 text-tertiary"
                    src={`${BASE}/bridge/down.svg`}
                  />
                </span>
              </div>
              <div className="flex flex-col gap-0.5 px-4 py-2">
                <p className="text-[16px] leading-5 text-muted-foreground">
                  You receive
                </p>
                <div className="flex items-center">
                  <output
                    aria-label="You receive amount"
                    className="min-w-0 flex-1 truncate text-[40px] font-semibold leading-[48px] tracking-[-1px] max-[480px]:text-[32px]"
                  >
                    {formatAmount(received)}
                  </output>
                  <TokenPill
                    asset={receiveAsset}
                    chain={chain.id}
                    label="Select asset to receive"
                    network={chain.network}
                    onClick={() => openPicker("receive")}
                  />
                </div>
                <p className="text-[16px] leading-5 text-muted-foreground">
                  ~{dollars(received * (11971.21 / 11985))}
                </p>
              </div>
            </div>
            <div className="px-2 pb-2">
              <div
                className={`flex items-center rounded-2xl pr-2 ${
                  picker === "recipient" ? "bg-accent" : ""
                }`}
              >
                <button
                  className="t-hover flex min-h-[60px] min-w-0 flex-1 items-center gap-3 rounded-2xl px-4 text-left hover:bg-accent"
                  onClick={() => openPicker("recipient")}
                  type="button"
                >
                  <BridgeWalletIcon />
                  <span className="flex min-w-0 flex-1 flex-col gap-1">
                    <span className="text-[13px] leading-4 text-muted-foreground">
                      {chain.network} recipient
                    </span>
                    <span
                      className={`truncate text-[20px] font-medium leading-6 ${
                        recipient ? "" : "text-tertiary"
                      }`}
                    >
                      {recipient
                        ? shortBridgeAddress(recipient)
                        : `Enter ${chain.network} address`}
                    </span>
                  </span>
                  {!recipient && (
                    <ThemedIcon
                      className="size-6 shrink-0 text-muted-foreground"
                      src={`${BASE}/icon-chevron-right.svg`}
                    />
                  )}
                </button>
                {recipient && (
                  <button
                    aria-label="Remove recipient"
                    className="t-hover flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
                    onClick={() => {
                      setDestination((current) => ({
                        ...current,
                        recipient: "",
                      }));
                      setPreviewMessage(false);
                    }}
                    type="button"
                  >
                    <ThemedIcon
                      className="size-6 text-muted-foreground"
                      src={`${BASE}/icon-cross.svg`}
                    />
                  </button>
                )}
              </div>
            </div>
            {recipient && hasAmount && (
              <dl
                className="mx-2 mb-2 rounded-2xl bg-accent px-4 py-1 text-[13px] leading-4"
                aria-label="Bridge quote preview"
              >
                {[
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
                ].map(([label, value]) => (
                  <div
                    className="flex justify-between gap-3 py-2.5"
                    key={label}
                  >
                    <dt className="text-muted-foreground">{label}</dt>
                    <dd className="text-right">{value}</dd>
                  </div>
                ))}
              </dl>
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
            <button
              className={`t-hover flex h-12 w-full items-center justify-center rounded-full px-3 text-[16px] font-medium leading-5 ${
                recipient && hasAmount
                  ? "bg-foreground text-background hover:opacity-90"
                  : "bg-destructive/[0.14] text-destructive"
              }`}
              onClick={() => {
                if (!hasAmount) amountRef.current?.focus();
                else if (!recipient) openPicker("recipient");
                else setPreviewMessage(true);
              }}
              type="button"
            >
              <TextSwap
                text={
                  !hasAmount
                    ? "Enter amount"
                    : !recipient
                    ? "Enter recipient"
                    : "Send now"
                }
              />
            </button>
          </footer>
        </PaneReveal>
      </section>
      {desktop ? (
        <aside className="h-full w-[400px] shrink-0">
          <InlineSheetReveal
            className="h-full overflow-clip rounded-3xl bg-card"
            isOpen={picker !== null}
          >
            <PaneReveal key={view}>{pickerContent}</PaneReveal>
          </InlineSheetReveal>
        </aside>
      ) : (
        <SheetReveal
          isOpen={picker !== null}
          label={title}
          onClose={closePicker}
          scrimClassName="fixed inset-0 z-50 flex bg-black/20 p-2 backdrop-blur-[4px] max-[795px]:bg-white/60 max-[795px]:p-0 max-[795px]:pt-8"
          sheetClassName="ml-auto flex h-full w-[392px] min-w-0 max-w-full flex-col overflow-clip rounded-3xl bg-card max-[795px]:w-full max-[795px]:rounded-b-none max-[795px]:shadow-[0px_-10px_40px_-10px_rgba(0,0,0,0.2)]"
        >
          <PaneReveal key={view}>{pickerContent}</PaneReveal>
        </SheetReveal>
      )}
    </>
  );
}
