"use client";

import type { EvmDepositNetwork, EvmDepositToken } from "@loyal-labs/api";
import { Search } from "lucide-react";
import { QRCodeSVG } from "qrcode.react";
import { useEffect, useRef, useState, type ReactNode } from "react";

import { copyTextToClipboard } from "./copy-text";
import { useEscapeOverlay } from "./escape-owner";
import { SheetReveal } from "./sheet-reveal";
import { SlidingTabs } from "./sliding-tabs";
import { TextSwap } from "./text-swap";
import { ThemedIcon } from "./themed-icon";
import { InfoTooltip } from "./info-tooltip";
import { useEvmDepositAddress } from "./use-evm-deposit-address";
import { useIsNarrowViewport } from "./use-is-narrow-viewport";

const BASE = "/wallet-workspace/facelift";
const TABS = ["Solana", "Other chains"] as const;
type Tab = (typeof TABS)[number];
type View = "receive" | "deposit-assets" | "chains";
const SEARCH_MIN_ITEMS = 10;

// Bridge stays off (#641) unless this browser opts in with
// localStorage["loyal.bridge"] = "1".
function bridgeOptedIn() {
  try {
    return localStorage.getItem("loyal.bridge") === "1";
  } catch {
    return false;
  }
}

type Selection = { asset: string; chain: string };

const RECEIVE = {
  symbol: "USDC",
  icon: "usdc.png",
  chain: "Solana",
  chainIcon: "sol.png",
};

const tokenOn = (network: EvmDepositNetwork, asset: string) =>
  network.tokens?.find((token) => token.asset === asset);

function minDeposit(minUsd: number, token: EvmDepositToken) {
  return token.stable
    ? `${minUsd.toFixed(2)} ${token.symbol}`
    : `$${minUsd.toFixed(2)}`;
}

function AssetIcon({
  file,
  label,
  size = 44,
}: {
  file: string | null;
  label: string;
  size?: number;
}) {
  if (!file) {
    return (
      <span
        aria-hidden="true"
        className="flex shrink-0 items-center justify-center rounded-full bg-accent-selected font-medium text-muted-foreground"
        style={{ height: size, width: size, fontSize: size / 3 }}
      >
        {label.slice(0, 1).toUpperCase()}
      </span>
    );
  }
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

function BadgedIcon({
  file,
  label,
  badge,
  badgeLabel,
}: {
  file: string | null;
  label: string;
  badge?: string | null;
  badgeLabel?: string;
}) {
  return (
    <span className="relative flex size-11 shrink-0">
      <AssetIcon file={file} label={label} />
      {badgeLabel && (
        <span className="absolute -bottom-0.5 -right-0.5 flex rounded-full bg-card p-0.5">
          <AssetIcon file={badge ?? null} label={badgeLabel} size={16} />
        </span>
      )}
    </span>
  );
}

function EmptyAssetIcon() {
  return (
    <span className="flex size-11 shrink-0 items-center justify-center rounded-full bg-accent-selected text-tertiary">
      <svg
        aria-hidden="true"
        fill="currentColor"
        height="24"
        viewBox="0 0 24 24"
        width="24"
      >
        <circle cx="9" cy="9.5" opacity="0.6" r="4.5" />
        <circle cx="14.5" cy="14.5" r="5" />
      </svg>
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
          aria-label={placeholder}
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

function Warning({
  children,
  onRetry,
}: {
  children: ReactNode;
  onRetry?: () => void;
}) {
  return (
    <div className="flex items-start rounded-2xl bg-destructive/[0.14] px-4 py-2 text-destructive">
      <ThemedIcon
        className="mr-2 size-6 shrink-0"
        src={`${BASE}/receive/warning.svg`}
      />
      <p className="min-w-0 flex-1 text-[13px] leading-4">{children}</p>
      {onRetry && (
        <button
          className="t-hover ml-2 shrink-0 rounded-full bg-accent-selected px-3 text-[13px] leading-6 text-foreground"
          onClick={onRetry}
          type="button"
        >
          Try again
        </button>
      )}
    </div>
  );
}

function AddressCard({
  address,
  bridge,
  heading,
  emptyLabel = "No wallet connected",
  qrIcon,
  pill,
  onCopy,
}: {
  address: string | null;
  bridge: boolean;
  heading?: string;
  emptyLabel?: string;
  qrIcon: string | null;
  pill?: ReactNode;
  onCopy: () => void;
}) {
  return (
    <div
      className={`relative flex shrink-0 flex-col items-center bg-accent ${
        bridge ? "w-full rounded-2xl" : "w-64 rounded-3xl"
      }`}
    >
      {pill}
      <div className={`px-8 pb-5 ${pill ? "pt-5 max-[795px]:pt-8" : "pt-8"}`}>
        {address ? (
          <QRCodeSVG
            value={address}
            size={192}
            minVersion={5}
            level="M"
            bgColor="transparent"
            fgColor="var(--foreground)"
            imageSettings={
              qrIcon
                ? {
                    src: `${BASE}/receive/${qrIcon}`,
                    width: 36,
                    height: 36,
                    excavate: true,
                  }
                : undefined
            }
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
          bridge ? "w-64 max-[795px]:w-[236px]" : "w-full px-4"
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
          className="t-hover absolute bottom-3.5 right-3 flex size-11 items-center justify-center rounded-full hover:bg-accent-selected"
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

const CELL =
  "flex h-[60px] w-full items-center rounded-2xl px-4 text-left max-[795px]:px-2";

function CellLabel({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <span className="ml-3 flex min-w-0 flex-1 flex-col gap-1">
      <span className="text-[13px] leading-4 text-muted-foreground">
        {label}
      </span>
      <span className="flex items-center font-medium text-[16px] leading-5">
        {children}
      </span>
    </span>
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
  const [tabState, setTabState] = useState<{
    wallet: string | null;
    tab: Tab;
  }>({ wallet: walletAddress, tab: initialTab });
  const tab = tabState.wallet === walletAddress ? tabState.tab : initialTab;
  const setTab = (next: Tab) =>
    setTabState({ wallet: walletAddress, tab: next });
  const [view, setView] = useState<View>("receive");
  const [selection, setSelection] = useState<Selection | null>(null);
  const [listing, setListing] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [copied, setCopied] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  const bridge = tab === "Other chains";
  const evm = useEvmDepositAddress(walletAddress);
  const live = bridgeOptedIn() && evm.ready;
  useEffect(() => {
    if (isOpen && bridge && live) void evm.load();
  }, [bridge, evm, isOpen, live]);
  // A failed load retries on the next open, not on every render.
  useEffect(() => {
    if (!isOpen) evm.retryOnNextLoad();
  }, [evm, isOpen]);
  const narrow = useIsNarrowViewport();
  const title =
    view === "deposit-assets"
      ? "Select asset to deposit"
      : view === "chains"
      ? "Select chain"
      : "Receive";
  const goTo = (next: View) => {
    setQuery("");
    setView(next);
  };
  const goBack = () => goTo("receive");
  useEscapeOverlay(isOpen, () => (view === "receive" ? onClose() : goBack()));

  useEffect(() => {
    setTabState({ wallet: walletAddress, tab: initialTab });
    if (!isOpen) return;
    setView("receive");
    setQuery("");
    setSelection(null);
    setListing(null);
    setCopied(false);
    const previous =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    return () => previous?.focus({ preventScroll: true });
  }, [isOpen, initialTab, walletAddress]);
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
  const networks = evm.networks;
  const depositTokens = [
    ...new Map(
      networks.flatMap((item) =>
        (item.tokens ?? []).map((token) => [token.asset, token] as const)
      )
    ).values(),
  ];
  const listedNetworks = listing
    ? networks.filter((item) => tokenOn(item, listing))
    : [];
  const assetResults = depositTokens.filter((item) =>
    matches(query, item.name, item.symbol)
  );
  const chainResults = listedNetworks.filter((item) =>
    matches(query, item.name)
  );
  const network = selection
    ? networks.find((item) => item.chain === selection.chain)
    : undefined;
  const deposit =
    network && selection ? tokenOn(network, selection.asset) : undefined;
  const resolved = network && deposit;

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
                <div className="flex flex-col gap-2 p-2 max-[795px]:px-4">
                  {live && evm.error && (
                    <Warning onRetry={() => void evm.retry()}>
                      {evm.error}
                    </Warning>
                  )}
                  {resolved && (
                    <>
                      <AddressCard
                        address={evm.info?.address ?? null}
                        bridge
                        heading={`Send ${deposit.symbol} to this address`}
                        emptyLabel={
                          evm.error
                            ? "No deposit address yet"
                            : "Loading address…"
                        }
                        qrIcon={narrow ? network.icon : deposit.icon}
                        pill={
                          <button
                            className="t-hover mt-5 flex h-9 items-center gap-2 rounded-full bg-accent-selected pl-2 pr-3 text-[16px] leading-5 max-[795px]:hidden"
                            onClick={() => {
                              setListing(deposit.asset);
                              goTo("chains");
                            }}
                            type="button"
                          >
                            <AssetIcon
                              file={network.icon}
                              label={network.name}
                              size={24}
                            />
                            {network.name}
                            <ThemedIcon
                              className="size-4 text-muted-foreground"
                              src={`${BASE}/icon-chevron-right.svg`}
                            />
                          </button>
                        }
                        onCopy={handleCopy}
                      />
                      <Warning>
                        {narrow
                          ? `Send funds only via the ${network.name} network and not from an exchange.`
                          : `Send funds to this address only via the ${network.name} network.`}
                        {network.minUsd > 0 &&
                          ` The minimum amount is $${network.minUsd} in the selected asset.`}{" "}
                        Sending an unsupported token will result in permanent
                        loss of funds.
                      </Warning>
                    </>
                  )}
                  <div className="relative max-[795px]:-mx-2">
                    <div
                      aria-hidden="true"
                      className="pointer-events-none absolute left-[37px] top-[54px] z-10 h-4 w-0.5 bg-foreground/20 max-[795px]:left-[29px]"
                    />
                    <button
                      className={`t-hover ${CELL} hover:bg-accent`}
                      onClick={() => goTo("deposit-assets")}
                      type="button"
                    >
                      {deposit ? (
                        <BadgedIcon
                          file={deposit.icon}
                          label={deposit.symbol}
                          badge={network?.icon}
                          badgeLabel={network?.name}
                        />
                      ) : (
                        <EmptyAssetIcon />
                      )}
                      <CellLabel label="Asset to deposit">
                        {deposit ? (
                          <>
                            {deposit.symbol}
                            {network && <Chip>{network.name}</Chip>}
                          </>
                        ) : (
                          <span className="text-tertiary">Select asset</span>
                        )}
                      </CellLabel>
                      <ThemedIcon
                        className="size-6 text-muted-foreground"
                        src={`${BASE}/icon-chevron-right.svg`}
                      />
                    </button>
                    <div className={`${CELL} mt-1`}>
                      <BadgedIcon
                        file={RECEIVE.icon}
                        label={RECEIVE.symbol}
                        badge={RECEIVE.chainIcon}
                        badgeLabel={RECEIVE.chain}
                      />
                      <CellLabel label="Asset to receive">
                        {RECEIVE.symbol}
                        <Chip>{RECEIVE.chain}</Chip>
                      </CellLabel>
                      <span className="flex size-6 items-center justify-center">
                        <InfoTooltip tone="dark" text="The asset you’ll receive after bridging. Transferred assets will be automatically converted to this asset." />
                      </span>
                    </div>
                  </div>
                  {resolved && (network.minUsd > 0 || deposit.feePercent) && (
                    <dl className="rounded-2xl bg-accent py-1 text-[13px] leading-4">
                      {network.minUsd > 0 && (
                        <div className="flex h-9 items-center justify-between px-4">
                          <dt className="text-muted-foreground">
                            Minimum deposit amount
                          </dt>
                          <dd>{minDeposit(network.minUsd, deposit)}</dd>
                        </div>
                      )}
                      {deposit.feePercent && (
                        <div className="flex h-9 items-center justify-between px-4">
                          <dt className="text-muted-foreground">Fee</dt>
                          <dd>{Number(deposit.feePercent)}%</dd>
                        </div>
                      )}
                    </dl>
                  )}
                </div>
              </div>
            ) : (
              <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
                <div className="flex min-h-[332px] flex-1 items-center justify-center p-4">
                  <AddressCard
                    address={walletAddress}
                    bridge={false}
                    qrIcon="mascot.svg"
                    onCopy={handleCopy}
                  />
                </div>
                <p className="mx-auto w-full max-w-[382px] shrink-0 px-8 pb-3 text-center text-[15px] leading-5 text-muted-foreground">
                  Use to receive assets on the Solana network only. Other assets
                  will be lost forever.
                </p>
              </div>
            )}
            {(!bridge || resolved) && (
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
            {((view === "deposit-assets" &&
              depositTokens.length >= SEARCH_MIN_ITEMS) ||
              (view === "chains" &&
                listedNetworks.length >= SEARCH_MIN_ITEMS)) && (
              <SearchField
                placeholder={
                  view === "chains" ? "Search chains" : "Search assets"
                }
                value={query}
                onChange={setQuery}
              />
            )}
            <div className="min-h-0 flex-1 overflow-y-auto p-2">
              {(view === "deposit-assets" || view === "chains") &&
                networks.length === 0 &&
                (evm.error ? (
                  <Warning onRetry={() => void evm.retry()}>
                    {evm.error}
                  </Warning>
                ) : (
                  evm.loading && (
                    <p className="px-4 py-3 text-[16px] leading-5 text-muted-foreground">
                      Loading…
                    </p>
                  )
                ))}
              {view === "deposit-assets" &&
                assetResults.map((item) => (
                  <button
                    className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                    key={item.asset}
                    onClick={() => {
                      setListing(item.asset);
                      setCopied(false);
                      goTo("chains");
                    }}
                    type="button"
                  >
                    <AssetIcon file={item.icon} label={item.symbol} />
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
              {view === "chains" &&
                listing &&
                chainResults.map((item) => {
                  const token = tokenOn(item, listing);
                  return (
                    <button
                      className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                      key={item.chain}
                      onClick={() => {
                        setSelection({ asset: listing, chain: item.chain });
                        setCopied(false);
                        goTo("receive");
                      }}
                      type="button"
                    >
                      <AssetIcon file={item.icon} label={item.name} />
                      <span className="ml-3 flex flex-1 flex-col gap-0.5">
                        <span className="font-medium text-[16px] leading-5">
                          {item.name}
                        </span>
                        {token && item.minUsd > 0 && (
                          <span className="text-[13px] leading-4 text-muted-foreground">
                            Min deposit amount:{" "}
                            {minDeposit(item.minUsd, token)}
                          </span>
                        )}
                      </span>
                    </button>
                  );
                })}
              {query.trim() &&
                (view === "chains" ? chainResults : assetResults).length ===
                  0 && (
                  <div className="flex h-[60px] items-center px-4">
                    <span
                      aria-hidden="true"
                      className="size-11 shrink-0 rounded-full border-2 border-dashed border-border"
                    />
                    <p className="ml-3 text-[16px] leading-5 text-muted-foreground">
                      No results found
                    </p>
                  </div>
                )}
            </div>
          </>
        )}
      </div>
    </SheetReveal>
  );
}
