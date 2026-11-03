"use client";

import { QRCodeSVG } from "qrcode.react";
import { useEffect, useRef, useState, type ReactNode } from "react";

import {
  CHAINS,
  SOLANA_ASSETS as RECEIVE_ASSETS,
  BRIDGE_ASSETS as DEPOSIT_ASSETS,
  type BridgeAsset,
} from "./bridge-preview-data";
import { copyTextToClipboard } from "./copy-text";
import { useEscapeOverlay } from "./escape-owner";
import { SheetReveal } from "./sheet-reveal";
import { SlidingTabs } from "./sliding-tabs";
import { TextSwap } from "./text-swap";
import { ThemedIcon } from "./themed-icon";

const BASE = "/wallet-workspace/facelift";
const TABS = ["Solana", "Other chains"] as const;
type Tab = (typeof TABS)[number];
type View = "receive" | "chains" | "assets" | "deposit-assets";
const BRIDGE_PREVIEW_ENABLED = process.env.NODE_ENV === "development";

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

function BridgeRow({
  label,
  value,
  icon,
  onClick,
  placeholder = false,
  badge,
}: {
  label: string;
  value: string;
  icon: ReactNode;
  onClick: () => void;
  placeholder?: boolean;
  badge?: string;
}) {
  return (
    <button
      className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
      onClick={onClick}
      type="button"
    >
      {icon}
      <span className="ml-3 flex min-w-0 flex-1 flex-col gap-1">
        <span className="text-[13px] leading-4 text-muted-foreground">
          {label}
        </span>
        <span
          className={`flex items-center gap-1 text-[16px] font-medium leading-5 ${
            placeholder ? "text-tertiary" : ""
          }`}
        >
          {value}
          {badge && (
            <span className="rounded-md bg-accent px-1 py-px text-[11px] leading-[14px] tracking-[0.06px] text-muted-foreground">
              {badge}
            </span>
          )}
        </span>
      </span>
      <ThemedIcon
        className="ml-3 size-6 shrink-0 text-muted-foreground"
        src={`${BASE}/icon-chevron-right.svg`}
      />
    </button>
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
  depositSymbol,
  onCopy,
}: {
  address: string | null;
  bridge: boolean;
  chainIcon: string;
  depositSymbol?: string;
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
      {bridge && (
        <p className="mb-1 text-center text-[16px] font-semibold leading-5">
          Send {depositSymbol} to this address
        </p>
      )}
      <div
        className={`flex w-full items-center pb-4 ${
          bridge ? "gap-2 px-4" : "px-4"
        }`}
      >
        {bridge && <span aria-hidden="true" className="size-10 shrink-0" />}
        <p className="min-w-0 flex-1 break-all text-center font-mono text-[16px] leading-5 text-muted-foreground">
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
            className="t-hover flex size-10 shrink-0 items-center justify-center rounded-full hover:bg-accent"
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
    </div>
  );
}

export function BridgePreviewReceiveSheet({
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
  const [tab, setTab] = useState<Tab>(
    BRIDGE_PREVIEW_ENABLED ? initialTab : "Solana"
  );
  const [view, setView] = useState<View>("receive");
  const [chain, setChain] = useState<(typeof CHAINS)[number] | null>(null);
  const [depositAsset, setDepositAsset] = useState<BridgeAsset | null>(null);
  const [search, setSearch] = useState("");
  const [asset, setAsset] = useState<BridgeAsset>(RECEIVE_ASSETS[0]);
  const [copied, setCopied] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  const bridge = tab === "Other chains";
  const bridgeReady = depositAsset !== null && chain !== null;
  const query = search.trim().toLowerCase();
  const filteredChains = CHAINS.filter((item) =>
    item.name.toLowerCase().includes(query)
  );
  const filteredAssets = (
    view === "deposit-assets" ? DEPOSIT_ASSETS : RECEIVE_ASSETS
  ).filter((item) =>
    `${item.name} ${item.symbol}`.toLowerCase().includes(query)
  );
  const title =
    view === "chains"
      ? "Select chain"
      : view === "assets"
      ? "Select asset to receive"
      : view === "deposit-assets"
      ? "Select asset to deposit"
      : "Receive";
  useEscapeOverlay(isOpen, () =>
    view === "receive"
      ? onClose()
      : setView(view === "chains" && !chain ? "deposit-assets" : "receive")
  );

  useEffect(() => {
    if (!isOpen) return;
    setTab(BRIDGE_PREVIEW_ENABLED ? initialTab : "Solana");
    setView("receive");
    setCopied(false);
    const previous =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    return () => previous?.focus({ preventScroll: true });
  }, [isOpen, initialTab]);
  useEffect(() => setSearch(""), [view, isOpen]);
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
        ...panelRef.current.querySelectorAll<HTMLElement>(
          "button:not(:disabled), input:not(:disabled)"
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
  const isTron = chain?.id === "tron";
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
              aria-label={
                view === "chains" && !chain
                  ? "Back to select asset"
                  : "Back to receive"
              }
              className="t-hover flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
              onClick={() =>
                setView(
                  view === "chains" && !chain ? "deposit-assets" : "receive"
                )
              }
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
                disabledTabs={BRIDGE_PREVIEW_ENABLED ? [] : ["Other chains"]}
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
                    className={`pointer-events-none absolute left-[45px] top-[63px] w-0.5 bg-foreground/20 ${
                      depositAsset ? "h-2.5" : "h-[18px]"
                    }`}
                  />
                  {depositAsset && (
                    <div
                      aria-hidden="true"
                      className="pointer-events-none absolute left-[45px] top-[123px] h-2.5 w-0.5 bg-foreground/20"
                    />
                  )}
                  <BridgeRow
                    label="Asset to deposit"
                    value={depositAsset?.symbol ?? "Select asset"}
                    placeholder={!depositAsset}
                    onClick={() => setView("deposit-assets")}
                    icon={
                      depositAsset ? (
                        <AssetIcon file={depositAsset.icon} />
                      ) : (
                        <span className="flex size-11 items-center justify-center rounded-full bg-accent">
                          <ThemedIcon
                            className="size-6 text-tertiary"
                            src={`${BASE}/receive/asset-placeholder.svg`}
                          />
                        </span>
                      )
                    }
                  />
                  {depositAsset && (
                    <BridgeRow
                      label="Sending from chain"
                      value={chain?.name ?? "Select chain"}
                      placeholder={!chain}
                      onClick={() => setView("chains")}
                      icon={
                        chain ? (
                          <AssetIcon file={`${chain.id}.png`} />
                        ) : (
                          <span className="flex size-11 items-center justify-center rounded-full bg-accent">
                            <ThemedIcon
                              className="size-6 text-tertiary"
                              src={`${BASE}/receive/assets.svg`}
                            />
                          </span>
                        )
                      }
                    />
                  )}
                  <div className={depositAsset ? "" : "mt-2"}>
                    <BridgeRow
                      label="Asset to receive"
                      value={asset.symbol}
                      onClick={() => setView("assets")}
                      badge="Solana"
                      icon={
                        <span className="relative size-11 shrink-0">
                          <AssetIcon file={asset.icon} />
                          <span className="absolute bottom-0 right-0 rounded-full border-2 border-card">
                            <AssetIcon file="sol.png" size={12} />
                          </span>
                        </span>
                      }
                    />
                  </div>
                </div>
                {bridgeReady && (
                  <div className="flex flex-col gap-2 p-2">
                    <Warning>
                      Send funds to this address only via the {chain.network}{" "}
                      network. The minimum amount is $1 in the selected asset.
                      Sending an unsupported token will result in permanent loss
                      of funds.
                    </Warning>
                    <AddressCard
                      address={exampleAddress}
                      bridge
                      chainIcon={
                        chain.id === "ethereum"
                          ? "ethereum-qr.png"
                          : `${chain.id}.png`
                      }
                      depositSymbol={depositAsset.symbol}
                      onCopy={handleCopy}
                    />
                    <dl className="rounded-2xl bg-accent py-1 text-[13px] leading-4">
                      <div className="flex justify-between px-4 py-2.5">
                        <dt className="text-muted-foreground">
                          Minimum deposit amount
                        </dt>
                        <dd>1.00 {depositAsset.symbol}</dd>
                      </div>
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
                )}
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
            {(!bridge || bridgeReady) && (
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
            )}
          </>
        ) : (
          <>
            <div className="shrink-0 px-4 pb-2">
              <div className="flex items-center rounded-full bg-accent px-3">
                <ThemedIcon
                  className="mr-3 size-6 shrink-0 text-muted-foreground"
                  src={`${BASE}/receive/search.svg`}
                />
                <input
                  aria-label={
                    view === "chains" ? "Search chains" : "Search assets"
                  }
                  className="min-w-0 flex-1 bg-transparent py-3 text-[16px] leading-5 outline-none placeholder:text-muted-foreground"
                  onChange={(event) => setSearch(event.target.value)}
                  placeholder={
                    view === "chains" ? "Search chains" : "Search assets"
                  }
                  type="search"
                  value={search}
                />
              </div>
            </div>
            <div className="min-h-0 flex-1 overflow-y-auto p-2">
              {view === "chains"
                ? filteredChains.map((item) => (
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
                      <span className="ml-3 font-medium text-[16px] leading-5 tracking-[-0.176px]">
                        {item.name}
                      </span>
                    </button>
                  ))
                : filteredAssets.map((item) => (
                    <button
                      className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                      key={item.symbol}
                      onClick={() => {
                        if (view === "deposit-assets") {
                          setDepositAsset(item);
                          setChain(null);
                          setCopied(false);
                          setView("chains");
                        } else {
                          setAsset(item);
                          setView("receive");
                        }
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
                    </button>
                  ))}
              {(view === "chains" ? filteredChains : filteredAssets).length ===
                0 && (
                <p
                  role="status"
                  className="px-4 py-8 text-center text-[14px] leading-5 text-muted-foreground"
                >
                  No {view === "chains" ? "chains" : "assets"} found
                </p>
              )}
            </div>
          </>
        )}
      </div>
    </SheetReveal>
  );
}
