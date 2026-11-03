"use client";

import type { EvmDepositNetwork } from "@loyal-labs/api";
import { Search } from "lucide-react";
import { QRCodeSVG } from "qrcode.react";
import { useEffect, useRef, useState, type ReactNode } from "react";

import { copyTextToClipboard } from "./copy-text";
import { useEscapeOverlay } from "./escape-owner";
import { SheetReveal } from "./sheet-reveal";
import { SlidingTabs } from "./sliding-tabs";
import { TextSwap } from "./text-swap";
import { ThemedIcon } from "./themed-icon";
import { useEvmDepositAddress } from "./use-evm-deposit-address";

const BASE = "/wallet-workspace/facelift";
const TABS = ["Solana", "Other chains"] as const;
type Tab = (typeof TABS)[number];
type View = "receive" | "deposit-assets" | "chains" | "receive-assets";

// Bridge stays off (#641) unless this browser opts in with
// localStorage["loyal.bridge"] = "1".
function bridgeOptedIn() {
  try {
    return localStorage.getItem("loyal.bridge") === "1";
  } catch {
    return false;
  }
}

const ASSET_DISPLAY: Record<
  string,
  { symbol: string; name: string; icon: string }
> = {
  usdc: { symbol: "USDC", name: "USD Coin", icon: "usdc.png" },
  usdt: { symbol: "USDT", name: "Tether USD", icon: "usdt.png" },
  eth: { symbol: "ETH", name: "Ether", icon: "ethereum.png" },
};

function assetDisplay(code: string) {
  return (
    ASSET_DISPLAY[code] ?? {
      symbol: code.toUpperCase(),
      name: code.toUpperCase(),
      icon: "assets.svg",
    }
  );
}

// Privy spells BNB Chain "bsc"; its icon file is bnb.png.
function chainIcon(chain: string) {
  return `${chain === "bsc" ? "bnb" : chain}.png`;
}

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

function BadgedIcon({ file, badge }: { file: string | null; badge?: string }) {
  return (
    <span className="relative flex size-11 shrink-0">
      {file ? (
        <AssetIcon file={file} />
      ) : (
        <span className="size-11 rounded-full bg-accent-selected" />
      )}
      {badge && (
        <span className="absolute -bottom-0.5 -right-0.5 flex rounded-full bg-card p-px">
          <AssetIcon file={badge} size={16} />
        </span>
      )}
    </span>
  );
}

function Chip({ children }: { children: ReactNode }) {
  return (
    <span className="ml-1.5 rounded-md bg-accent px-1.5 py-px text-[11px] font-normal leading-[14px] text-muted-foreground">
      {children}
    </span>
  );
}

function SearchField({
  placeholder,
  value,
  onChange,
}: {
  placeholder: string;
  value: string;
  onChange: (value: string) => void;
}) {
  return (
    <div className="shrink-0 px-4 pb-2">
      <div className="flex w-full items-center rounded-full bg-accent px-3">
        <Search
          className="mr-3 shrink-0 text-muted-foreground"
          size={24}
          strokeWidth={2}
        />
        <input
          className="min-w-0 flex-1 bg-transparent py-3 text-[16px] text-foreground leading-5 outline-none placeholder:text-muted-foreground"
          onChange={(event) => onChange(event.target.value)}
          placeholder={placeholder}
          type="text"
          value={value}
        />
      </div>
    </div>
  );
}

function matches(query: string, ...texts: string[]) {
  const needle = query.trim().toLowerCase();
  return texts.some((text) => text.toLowerCase().includes(needle));
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
  heading,
  emptyLabel = "No wallet connected",
  chainIcon,
  onCopy,
}: {
  address: string | null;
  bridge: boolean;
  heading?: string;
  emptyLabel?: string;
  chainIcon: string;
  onCopy: () => void;
}) {
  return (
    <div
      className={`relative flex shrink-0 flex-col items-center bg-accent ${
        bridge ? "w-full rounded-2xl" : "w-64 rounded-3xl"
      }`}
    >
      <div className="px-8 pt-8 pb-5">
        {address ? (
          <QRCodeSVG
            value={address}
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
      {heading && (
        <p className="pb-1 text-center font-semibold text-[16px] leading-5">
          {heading}
        </p>
      )}
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
          emptyLabel
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
  const [assetCode, setAssetCode] = useState<string | null>(null);
  const [chainId, setChainId] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [copied, setCopied] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  const bridge = tab === "Other chains";
  const evm = useEvmDepositAddress(walletAddress);
  const live = bridgeOptedIn() && evm.eligible;
  useEffect(() => {
    if (isOpen && bridge && live) void evm.load();
  }, [bridge, evm, isOpen, live]);
  // A failed load retries on the next open, not on every render.
  useEffect(() => {
    if (!isOpen) evm.retryOnNextLoad();
  }, [evm, isOpen]);
  const title =
    view === "deposit-assets"
      ? "Select asset to deposit"
      : view === "chains"
      ? "Select chain"
      : view === "receive-assets"
      ? "Select asset to receive"
      : "Receive";
  const goTo = (next: View) => {
    setQuery("");
    setView(next);
  };
  const goBack = () => goTo(view === "chains" ? "deposit-assets" : "receive");
  useEscapeOverlay(isOpen, () => (view === "receive" ? onClose() : goBack()));

  useEffect(() => {
    if (!isOpen) return;
    setTab(initialTab);
    setView("receive");
    setQuery("");
    setAssetCode(null);
    setChainId(null);
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
    const address = bridge ? evm.info?.address : walletAddress;
    if (address) void copyTextToClipboard(address).then(setCopied);
  };
  const networks = evm.info?.networks ?? [];
  const depositAssets = [
    ...new Set(networks.flatMap((item) => item.assets ?? [])),
  ];
  const assetNetworks = networks.filter((item) =>
    assetCode ? item.assets?.includes(assetCode) : false
  );
  const network: EvmDepositNetwork | undefined = assetNetworks.find(
    (item) => item.chain === chainId
  );
  const deposit = assetCode ? assetDisplay(assetCode) : null;
  const receive = assetDisplay("usdc");

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
              aria-label="Back"
              className="t-hover flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
              onClick={goBack}
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
                disabledTabs={live ? [] : ["Other chains"]}
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
                    className="pointer-events-none absolute left-[45px] top-[60px] z-10 h-4 w-0.5 bg-foreground/20"
                  />
                  <button
                    className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                    onClick={() => goTo("deposit-assets")}
                    type="button"
                  >
                    <BadgedIcon
                      file={deposit?.icon ?? null}
                      badge={network ? chainIcon(network.chain) : undefined}
                    />
                    <span className="ml-3 flex min-w-0 flex-1 flex-col gap-1">
                      <span className="text-[13px] leading-4 text-muted-foreground">
                        Asset to deposit
                      </span>
                      <span className="flex items-center font-medium text-[16px] leading-5">
                        {deposit ? (
                          <>
                            {deposit.symbol}
                            {network && <Chip>{network.name}</Chip>}
                          </>
                        ) : (
                          <span className="text-muted-foreground">
                            Select asset
                          </span>
                        )}
                      </span>
                    </span>
                    <ThemedIcon
                      className="size-6 text-muted-foreground"
                      src={`${BASE}/icon-chevron-right.svg`}
                    />
                  </button>
                  <button
                    className="t-hover mt-4 flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                    onClick={() => goTo("receive-assets")}
                    type="button"
                  >
                    <BadgedIcon file={receive.icon} badge="sol.png" />
                    <span className="ml-3 flex min-w-0 flex-1 flex-col gap-1">
                      <span className="text-[13px] leading-4 text-muted-foreground">
                        Asset to receive
                      </span>
                      <span className="flex items-center font-medium text-[16px] leading-5">
                        {receive.symbol}
                        <Chip>Solana</Chip>
                      </span>
                    </span>
                    <ThemedIcon
                      className="size-6 text-muted-foreground"
                      src={`${BASE}/icon-chevron-right.svg`}
                    />
                  </button>
                </div>
                {live && evm.error && (
                  <div className="p-2">
                    <Warning>{evm.error}</Warning>
                  </div>
                )}
                {network && deposit && (
                  <div className="flex flex-col gap-2 p-2">
                    <Warning>
                      Send funds only via the {network.name} network and not
                      from an exchange.
                      {network.minUsd > 0 &&
                        ` The minimum amount is $${network.minUsd} in the selected asset.`}{" "}
                      Sending an unsupported token will result in permanent loss
                      of funds.
                    </Warning>
                    <AddressCard
                      address={evm.info?.address ?? null}
                      bridge
                      heading={`Send ${deposit.symbol} to this address`}
                      emptyLabel={
                        evm.error ? "No deposit address yet" : "Loading address…"
                      }
                      chainIcon={chainIcon(network.chain)}
                      onCopy={handleCopy}
                    />
                    {network.minUsd > 0 && (
                      <dl className="rounded-2xl bg-accent py-1 text-[13px] leading-4">
                        <div className="flex justify-between px-4 py-2.5">
                          <dt className="text-muted-foreground">
                            Minimum deposit amount
                          </dt>
                          <dd>${network.minUsd}</dd>
                        </div>
                      </dl>
                    )}
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
            {(!bridge || network) && (
              <div className="shrink-0 px-4 pt-2 pb-4">
                <button
                  className="t-hover flex h-12 w-full items-center justify-center rounded-full bg-accent text-[16px] font-medium leading-5 enabled:hover:bg-accent-selected disabled:opacity-40"
                  disabled={!(bridge ? evm.info?.address : walletAddress)}
                  onClick={handleCopy}
                  type="button"
                >
                  <TextSwap text={copied ? "Copied!" : "Copy address"} />
                </button>
              </div>
            )}
          </>
        ) : (
          <>
            {view === "deposit-assets" && (
              <SearchField
                placeholder="Search assets"
                value={query}
                onChange={setQuery}
              />
            )}
            {view === "chains" && (
              <SearchField
                placeholder="Search chains"
                value={query}
                onChange={setQuery}
              />
            )}
            <div className="min-h-0 flex-1 overflow-y-auto p-2">
              {view === "deposit-assets" &&
                depositAssets
                  .filter((code) =>
                    matches(query, assetDisplay(code).name, assetDisplay(code).symbol)
                  )
                  .map((code) => {
                    const item = assetDisplay(code);
                    return (
                      <button
                        className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                        key={code}
                        onClick={() => {
                          setAssetCode(code);
                          setChainId(null);
                          setCopied(false);
                          goTo("chains");
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
                    );
                  })}
              {view === "chains" &&
                assetNetworks
                  .filter((item) => matches(query, item.name))
                  .map((item) => (
                    <button
                      className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                      key={item.chain}
                      onClick={() => {
                        setChainId(item.chain);
                        setCopied(false);
                        goTo("receive");
                      }}
                      type="button"
                    >
                      <AssetIcon file={chainIcon(item.chain)} />
                      <span className="ml-3 font-medium text-[16px] leading-5">
                        {item.name}
                      </span>
                    </button>
                  ))}
              {view === "receive-assets" && (
                <>
                  <div className="mb-2 flex items-start rounded-2xl bg-accent px-4 py-2 text-muted-foreground">
                    <ThemedIcon
                      className="mr-2 size-4 shrink-0"
                      src={`${BASE}/icon-circle-info.svg`}
                    />
                    <p className="min-w-0 flex-1 text-[13px] leading-4">
                      The asset you&apos;ll receive after bridging. Transferred
                      assets will be automatically converted to the selected
                      asset.
                    </p>
                  </div>
                  <button
                    className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                    onClick={() => goTo("receive")}
                    type="button"
                  >
                    <BadgedIcon file={receive.icon} badge="sol.png" />
                    <span className="ml-3 flex flex-1 flex-col gap-0.5">
                      <span className="font-medium text-[16px] leading-5 tracking-[-0.176px]">
                        {receive.name}
                      </span>
                      <span className="text-[13px] leading-4 text-muted-foreground">
                        {receive.symbol}
                      </span>
                    </span>
                  </button>
                </>
              )}
            </div>
          </>
        )}
      </div>
    </SheetReveal>
  );
}
