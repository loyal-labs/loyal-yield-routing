"use client";

import { QRCodeSVG } from "qrcode.react";
import { useEffect, useRef, useState, type ReactNode } from "react";

import { copyTextToClipboard } from "./copy-text";
import { SplitUsd } from "./crypto-pane";
import { useBalanceVisibility } from "./balance-visibility";
import { useEscapeOverlay } from "./escape-owner";
import { SheetReveal } from "./sheet-reveal";
import { SlidingTabs } from "./sliding-tabs";
import { TextSwap } from "./text-swap";
import { ThemedIcon } from "./themed-icon";

const BASE = "/wallet-workspace/facelift";
const TABS = ["Solana", "Other chains"] as const;
type Tab = (typeof TABS)[number];
type View = "receive" | "chains" | "assets" | "supported";

// Figma fixtures only. Bridge has no backend, deposit address, or transaction flow yet.
const CHAINS = [
  { id: "ethereum", name: "Ethereum" },
  { id: "tron", name: "Tron" },
  { id: "bitcoin", name: "Bitcoin" },
  { id: "base", name: "Base" },
  { id: "arbitrum", name: "Arbitrum" },
  { id: "polygon", name: "Polygon" },
  { id: "bnb", name: "BNB Smart Chain" },
] as const;
const RECEIVE_ASSETS = [
  {
    symbol: "USDC",
    name: "USD Coin",
    icon: "usdc.png",
    value: "$9,750.00",
    amount: "9,750.00 USDC",
  },
  {
    symbol: "SOL",
    name: "Solana",
    icon: "sol.png",
    value: "$0.00",
    amount: "0.00 SOL",
  },
] as const;
const SUPPORTED_ASSETS = [
  { symbol: "USDT", name: "Tether USD", icon: "usdt.png" },
  { symbol: "TRX", name: "Tron", icon: "tron.png" },
];

function AssetIcon({ file, size = 44 }: { file: string; size?: number }) {
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      alt=""
      aria-hidden="true"
      className="shrink-0 rounded-full object-cover"
      height={size}
      width={size}
      src={`${BASE}/receive/${file}`}
    />
  );
}

function Warning({ children }: { children: ReactNode }) {
  return (
    <div className="flex items-start rounded-2xl bg-destructive/[0.14] px-4 py-2 text-destructive">
      <ThemedIcon
        className="mr-2 size-6 shrink-0"
        src={`${BASE}/receive/warning.svg`}
      />
      <p className="min-w-0 flex-1 text-[13px] leading-4">{children}</p>
    </div>
  );
}

function AddressCard({
  address,
  bridge,
  chainIcon,
  onCopy,
}: {
  address: string | null;
  bridge: boolean;
  chainIcon: string;
  onCopy: () => void;
}) {
  return (
    <div
      className={`relative flex shrink-0 flex-col items-center bg-accent ${
        bridge ? "w-full rounded-2xl" : "w-64 rounded-3xl"
      }`}
      title={bridge ? "Bridge design preview — example address" : undefined}
    >
      <div className="px-8 pt-8 pb-5">
        {address ? (
          <QRCodeSVG
            // Mock bridge QR intentionally contains no usable deposit address.
            value={bridge ? "loyal:bridge-design-preview" : address}
            size={192}
            minVersion={5}
            level="M"
            bgColor="transparent"
            fgColor="var(--foreground)"
            imageSettings={{
              src: `${BASE}/receive/${bridge ? chainIcon : "mascot.svg"}`,
              width: 36,
              height: 36,
              excavate: true,
            }}
          />
        ) : (
          <div className="size-48 rounded-2xl bg-accent-selected" />
        )}
      </div>
      <p
        className={`break-all pb-4 text-center font-mono text-[16px] leading-5 text-muted-foreground ${
          bridge ? "w-64" : "w-full px-4"
        }`}
      >
        {address ? (
          <>
            <span className="text-foreground">{address.slice(0, 4)}</span>
            {address.slice(4, -4)}
            <span className="text-foreground">{address.slice(-4)}</span>
          </>
        ) : (
          "No wallet connected"
        )}
      </p>
      {bridge && (
        <button
          aria-label="Copy bridge address"
          className="t-hover absolute right-2 top-2 flex size-11 items-center justify-center rounded-full hover:bg-accent"
          onClick={onCopy}
          type="button"
        >
          <ThemedIcon
            className="size-6 text-tertiary"
            src={`${BASE}/icon-copy.svg`}
          />
        </button>
      )}
    </div>
  );
}

export function ReceiveSheet({
  isOpen,
  onClose,
  walletAddress,
  initialTab = "Solana",
}: {
  isOpen: boolean;
  onClose: () => void;
  walletAddress: string | null;
  initialTab?: Tab;
}) {
  const [tab, setTab] = useState<Tab>(initialTab);
  const [view, setView] = useState<View>("receive");
  const [chain, setChain] = useState<(typeof CHAINS)[number]>(CHAINS[0]);
  const [asset, setAsset] = useState<(typeof RECEIVE_ASSETS)[number]>(
    RECEIVE_ASSETS[0]
  );
  const [copied, setCopied] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  const { isBalanceHidden } = useBalanceVisibility();
  const bridge = tab === "Other chains";
  const title =
    view === "chains"
      ? "Select chain"
      : view === "assets"
      ? "Select asset to receive"
      : view === "supported"
      ? "Supported assets"
      : "Receive";
  useEscapeOverlay(isOpen, () =>
    view === "receive" ? onClose() : setView("receive")
  );

  useEffect(() => {
    if (!isOpen) return;
    setTab(initialTab);
    setView("receive");
    setCopied(false);
    const previous =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    return () => previous?.focus({ preventScroll: true });
  }, [isOpen, initialTab]);
  useEffect(() => {
    if (!isOpen) return;
    // Prevent focus scrolling from cancelling the panel's entry translation.
    const frame = requestAnimationFrame(() =>
      panelRef.current
        ?.querySelector<HTMLButtonElement>("button")
        ?.focus({ preventScroll: true })
    );
    const trapFocus = (event: KeyboardEvent) => {
      if (
        event.key !== "Tab" ||
        !panelRef.current?.contains(event.target as Node)
      )
        return;
      const elements = [
        ...panelRef.current.querySelectorAll<HTMLButtonElement>(
          "button:not(:disabled)"
        ),
      ];
      const first = elements[0],
        last = elements.at(-1);
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last?.focus({ preventScroll: true });
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first?.focus({ preventScroll: true });
      }
    };
    window.addEventListener("keydown", trapFocus, true);
    return () => {
      cancelAnimationFrame(frame);
      window.removeEventListener("keydown", trapFocus, true);
    };
  }, [isOpen, view]);
  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 1800);
    return () => clearTimeout(timer);
  }, [copied]);

  const handleCopy = () => {
    if (bridge) {
      setCopied(true);
      return;
    }
    if (walletAddress) void copyTextToClipboard(walletAddress).then(setCopied);
  };
  const isTron = chain.id === "tron";
  const exampleAddress = isTron
    ? "TEFgdDE6vrxWNJfhEa3SJeWfVpwGdEC9Sh"
    : "0xadC45bc2270491091e9C454B9e68FbFE91aA7BCb";

  return (
    <SheetReveal
      label={title}
      isOpen={isOpen}
      onClose={onClose}
      scrimClassName="fixed inset-0 z-40 flex overflow-hidden items-start justify-end bg-black/20 p-2 max-[795px]:items-end max-[795px]:p-0"
      sheetClassName="t-settings-panel t-receive-panel flex h-full max-h-[calc(100dvh-16px)] w-[400px] min-w-0 flex-col overflow-hidden rounded-3xl bg-card max-[795px]:max-h-[95dvh] max-[795px]:w-full max-[795px]:rounded-b-none"
    >
      <div className="flex min-h-0 flex-1 flex-col" ref={panelRef}>
        <header className="flex shrink-0 items-center p-2">
          {view !== "receive" && (
            <button
              aria-label="Back to receive"
              className="t-hover flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
              onClick={() => setView("receive")}
              type="button"
            >
              <ThemedIcon
                className="size-6 text-muted-foreground"
                src={`${BASE}/icon-arrow-left.svg`}
              />
            </button>
          )}
          <h2
            className={`min-w-0 flex-1 truncate py-2.5 font-semibold text-[20px] leading-6 ${
              view === "receive" ? "pl-4" : "pl-3"
            }`}
          >
            {title}
          </h2>
          <button
            aria-label="Close receive"
            className="t-hover flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
            onClick={onClose}
            type="button"
          >
            <ThemedIcon
              className="size-6 text-muted-foreground"
              src={`${BASE}/icon-cross.svg`}
            />
          </button>
        </header>
        {view === "receive" ? (
          <>
            <div className="shrink-0 px-4 pb-2">
              <SlidingTabs
                activeTab={tab}
                onSelect={(next) => {
                  setTab(next);
                  setCopied(false);
                }}
                tabs={TABS}
                disabledTabs={["Other chains"]}
                className="w-full [&_.t-tab]:min-w-0 [&_.t-tab]:flex-1 [&_.t-tab]:px-2"
                renderLabel={(label) => (
                  <span className="flex items-center justify-center gap-1.5">
                    {label}
                    {label === "Other chains" && (
                      <span className="rounded-full bg-[#9946fc] px-1.5 py-px text-[11px] leading-[14px] tracking-[0.06px] text-white">
                        NEW
                      </span>
                    )}
                  </span>
                )}
              />
            </div>
            {bridge ? (
              <div className="min-h-0 flex-1 overflow-y-auto">
                <div className="relative p-2">
                  <div
                    aria-hidden="true"
                    className="pointer-events-none absolute left-[45px] top-[63px] z-10 h-[58px] w-0.5 bg-foreground/20"
                  />
                  <div className="group relative h-[100px] rounded-2xl hover:bg-accent">
                    <button
                      aria-label={`Sending from chain ${chain.name}`}
                      className="absolute inset-0 rounded-2xl"
                      onClick={() => setView("chains")}
                      type="button"
                    />
                    <div className="pointer-events-none flex items-start px-4 py-2">
                      <AssetIcon file={`${chain.id}.png`} />
                      <div className="ml-3 flex min-w-0 flex-1 flex-col gap-1 py-0.5">
                        <span className="text-[13px] leading-4 text-muted-foreground">
                          Sending from chain
                        </span>
                        <span className="font-medium text-[16px] leading-5">
                          {chain.name}
                        </span>
                        <button
                          aria-label={`${isTron ? 2 : 22} assets supported`}
                          className="t-hover pointer-events-auto relative z-10 flex w-fit items-center rounded-3xl bg-accent p-2 hover:bg-accent-selected"
                          onClick={() => setView("supported")}
                          type="button"
                        >
                          <span className="flex items-center pr-2 [&>img+img]:-ml-[5px]">
                            {(isTron
                              ? ["tron.png", "usdt.png"]
                              : ["usdg.svg", "pyusd.svg", "bitcoin.png"]
                            ).map((file) => (
                              <AssetIcon file={file} key={file} size={20} />
                            ))}
                          </span>
                          <span className="text-[13px] leading-4 text-muted-foreground">
                            {isTron ? 2 : 22} assets supported
                          </span>
                          <ThemedIcon
                            className="size-4 text-muted-foreground"
                            src={`${BASE}/icon-chevron-right.svg`}
                          />
                        </button>
                      </div>
                      <ThemedIcon
                        className="my-2.5 size-6 text-muted-foreground"
                        src={`${BASE}/icon-chevron-right.svg`}
                      />
                    </div>
                  </div>
                  <button
                    className="t-hover mt-1 flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                    onClick={() => setView("assets")}
                    type="button"
                  >
                    <AssetIcon file={asset.icon} />
                    <span className="ml-3 flex flex-1 flex-col gap-1">
                      <span className="text-[13px] leading-4 text-muted-foreground">
                        Receive asset
                      </span>
                      <span className="font-medium text-[16px] leading-5">
                        {asset.symbol}
                      </span>
                    </span>
                    <ThemedIcon
                      className="size-6 text-muted-foreground"
                      src={`${BASE}/icon-chevron-right.svg`}
                    />
                  </button>
                </div>
                <div className="flex flex-col gap-2 p-2">
                  <Warning>
                    Send funds only via the {chain.name} network and not from an
                    exchange. The minimum amount is ${isTron ? "2" : "1"} in the
                    selected asset. Sending an unsupported token will result in
                    permanent loss of funds.
                  </Warning>
                  <AddressCard
                    address={exampleAddress}
                    bridge
                    chainIcon={`${chain.id}.png`}
                    onCopy={handleCopy}
                  />
                  <dl className="rounded-2xl bg-accent py-1 text-[13px] leading-4">
                    <div className="flex justify-between px-4 py-2.5">
                      <dt className="text-muted-foreground">Fee</dt>
                      <dd>1%</dd>
                    </div>
                    <div className="flex justify-between px-4 py-2.5">
                      <dt className="text-muted-foreground">
                        Estimated deposit time
                      </dt>
                      <dd>~2 min</dd>
                    </div>
                  </dl>
                </div>
              </div>
            ) : (
              <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
                <div className="flex min-h-[332px] flex-1 items-center justify-center p-4">
                  <AddressCard
                    address={walletAddress}
                    bridge={false}
                    chainIcon="mascot.svg"
                    onCopy={handleCopy}
                  />
                </div>
                <p className="mx-auto w-full max-w-[382px] shrink-0 px-8 pb-3 text-center text-[15px] leading-5 text-muted-foreground">
                  Use to receive assets on the Solana network only. Other assets
                  will be lost forever.
                </p>
              </div>
            )}
            <div className="shrink-0 px-4 pt-2 pb-4">
              <button
                className="t-hover flex h-12 w-full items-center justify-center rounded-full bg-accent text-[16px] font-medium leading-5 enabled:hover:bg-accent-selected disabled:opacity-40"
                disabled={!bridge && !walletAddress}
                onClick={handleCopy}
                type="button"
              >
                <TextSwap
                  text={
                    copied
                      ? bridge
                        ? "Preview only"
                        : "Copied!"
                      : "Copy address"
                  }
                />
              </button>
            </div>
          </>
        ) : (
          <div className="min-h-0 flex-1 overflow-y-auto p-2">
            {view === "chains" ? (
              CHAINS.map((item) => (
                <button
                  className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                  key={item.id}
                  onClick={() => {
                    setChain(item);
                    setCopied(false);
                    setView("receive");
                  }}
                  type="button"
                >
                  <AssetIcon file={`${item.id}.png`} />
                  <span className="ml-3 font-medium text-[16px] leading-5">
                    {item.name}
                  </span>
                </button>
              ))
            ) : view === "assets" ? (
              RECEIVE_ASSETS.map((item) => (
                <button
                  className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                  key={item.symbol}
                  onClick={() => {
                    setAsset(item);
                    setView("receive");
                  }}
                  type="button"
                >
                  <AssetIcon file={item.icon} />
                  <span className="ml-3 flex flex-1 flex-col gap-0.5">
                    <span className="font-medium text-[16px] leading-5 tracking-[-0.176px]">
                      {item.name}
                    </span>
                    <span className="text-[13px] leading-4 text-muted-foreground">
                      {item.symbol}
                    </span>
                  </span>
                  <span className="flex flex-col items-end gap-0.5 pl-3">
                    <SplitUsd isHidden={isBalanceHidden} value={item.value} />
                    <span className="text-[13px] leading-4 text-muted-foreground">
                      {isBalanceHidden ? "••••" : item.amount}
                    </span>
                  </span>
                </button>
              ))
            ) : (
              <>
                <div className="mb-2">
                  <Warning>
                    Only send the supported assets listed below. Depositing any
                    unsupported asset may result in the permanent loss of your
                    funds.
                  </Warning>
                </div>
                {SUPPORTED_ASSETS.map((item) => (
                  <div
                    className="flex h-[60px] w-full items-center rounded-2xl px-4 hover:bg-accent"
                    key={item.symbol}
                  >
                    <AssetIcon file={item.icon} />
                    <span className="ml-3 flex flex-1 flex-col gap-0.5">
                      <span className="font-medium text-[16px] leading-5 tracking-[-0.176px]">
                        {item.name}
                      </span>
                      <span className="text-[13px] leading-4 text-muted-foreground">
                        {item.symbol}
                      </span>
                    </span>
                  </div>
                ))}
              </>
            )}
          </div>
        )}
      </div>
    </SheetReveal>
  );
}
