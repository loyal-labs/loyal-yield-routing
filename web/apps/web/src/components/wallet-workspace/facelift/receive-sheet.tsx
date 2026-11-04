"use client";

import type { EvmDepositNetwork, EvmDepositToken } from "@loyal-labs/api";
import { QRCodeSVG } from "qrcode.react";
import { useEffect, useRef, useState, type ReactNode } from "react";

import { copyTextToClipboard } from "./copy-text";
import { useEscapeOverlay } from "./escape-owner";
import { floorToCents } from "./floor-to-cents";
import { InfoTooltip } from "./info-tooltip";
import { SheetReveal } from "./sheet-reveal";
import { SlidingTabs } from "./sliding-tabs";
import { TextSwap } from "./text-swap";
import { ThemedIcon } from "./themed-icon";
import {
  fromRaw,
  type MoveSource,
  useBridgeMoveBalances,
} from "./use-bridge-move";
import { useEvmDepositAddress } from "./use-evm-deposit-address";
import { useIsNarrowViewport } from "./use-is-narrow-viewport";

const BASE = "/wallet-workspace/facelift";
const TABS = ["Solana", "Other chains"] as const;
type Tab = (typeof TABS)[number];
type View = "receive" | "chains" | "deposit-assets";
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

type Selection = { asset: string; chain: string | null };

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

function matches(query: string, ...texts: string[]) {
  const needle = query.trim().toLowerCase();
  return texts.some((text) => text.toLowerCase().includes(needle));
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
  badge: string | null;
  badgeLabel: string;
}) {
  return (
    <span className="relative size-11 shrink-0">
      <AssetIcon file={file} label={label} />
      <span className="absolute bottom-0 right-0 translate-x-0.5 translate-y-0.5 rounded-full border-2 border-card">
        <AssetIcon file={badge} label={badgeLabel} size={16} />
      </span>
    </span>
  );
}

function RowText({
  label,
  value,
  placeholder = false,
  badge,
}: {
  label: string;
  value: string;
  placeholder?: boolean;
  badge?: string;
}) {
  return (
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
      className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent max-[795px]:px-2"
      onClick={onClick}
      type="button"
    >
      {icon}
      <RowText
        label={label}
        value={value}
        placeholder={placeholder}
        badge={badge}
      />
      <ThemedIcon
        className="ml-3 size-6 shrink-0 text-muted-foreground"
        src={`${BASE}/icon-chevron-right.svg`}
      />
    </button>
  );
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
  qrIcon,
  depositSymbol,
  emptyLabel = "No wallet connected",
  onCopy,
}: {
  address: string | null;
  bridge: boolean;
  qrIcon: string | null;
  depositSymbol?: string;
  emptyLabel?: string;
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
            emptyLabel
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

export function ReceiveSheet({
  isOpen,
  onClose,
  walletAddress,
  initialTab = "Solana",
  onMove,
}: {
  isOpen: boolean;
  onClose: () => void;
  walletAddress: string | null;
  initialTab?: Tab;
  onMove: (source: MoveSource) => void;
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
  const moves = useBridgeMoveBalances(
    isOpen && bridge && evm.info?.kind === "relay_signed",
    evm.networks,
    walletAddress
  );
  const narrow = useIsNarrowViewport();
  const title =
    view === "chains"
      ? "Select chain"
      : view === "deposit-assets"
      ? "Select asset to deposit"
      : "Receive";
  const goTo = (next: View) => {
    setQuery("");
    setView(next);
  };
  const backToAsset = view === "chains" && !selection?.chain;
  const goBack = () => goTo(backToAsset ? "deposit-assets" : "receive");
  useEscapeOverlay(isOpen, () => (view === "receive" ? onClose() : goBack()));

  useEffect(() => {
    setTabState({ wallet: walletAddress, tab: initialTab });
    if (!isOpen) return;
    setView("receive");
    setQuery("");
    setSelection(null);
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

  const networks = evm.networks;
  const depositTokens = [
    ...new Map(
      networks.flatMap((item) =>
        (item.tokens ?? []).map((token) => [token.asset, token] as const)
      )
    ).values(),
  ];
  const depositAsset = depositTokens.find(
    (item) => item.asset === selection?.asset
  );
  const selectableNetworks = selection
    ? networks.filter((item) => tokenOn(item, selection.asset))
    : [];
  const filteredAssets = depositTokens.filter((item) =>
    matches(query, item.name, item.symbol)
  );
  const filteredChains = selectableNetworks.filter((item) =>
    matches(query, item.name)
  );
  const network = networks.find((item) => item.chain === selection?.chain);
  const deposit =
    network && selection ? tokenOn(network, selection.asset) : undefined;
  const resolved = network && deposit;
  const tronRoute = network?.route === "relay_deposit_address";
  const bridgeAddress = network?.address ?? null;
  const routeError = tronRoute ? evm.tronError : evm.error;
  const retryRoute = () => void (tronRoute ? evm.retryTron() : evm.retry());
  useEffect(() => {
    if (isOpen && bridge && live && tronRoute && !bridgeAddress) {
      void evm.mintTron();
    }
  }, [bridge, bridgeAddress, evm, isOpen, live, tronRoute]);
  const handleCopy = () => {
    const address = bridge ? bridgeAddress : walletAddress;
    if (address) void copyTextToClipboard(address).then(setCopied);
  };

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
              aria-label={backToAsset ? "Back to select asset" : "Back to receive"}
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
                {((live && routeError) || moves.error) && (
                  <div className="flex flex-col gap-2 px-2 pt-2 max-[795px]:px-4">
                    {live && routeError && (
                      <Warning onRetry={retryRoute}>{routeError}</Warning>
                    )}
                    {moves.error && (
                      <Warning onRetry={moves.reload}>{moves.error}</Warning>
                    )}
                  </div>
                )}
                {(moves.balances.length > 0 || moves.transfers.length > 0) && (
                  <div className="px-2 pt-2">
                    {moves.balances.map((item) => (
                      <BridgeRow
                        key={`${item.chain}:${item.asset}`}
                        label="Ready to move to Solana"
                        value={`${fromRaw(item.amountRaw, item.decimals)} ${item.symbol}`}
                        badge={item.chainName}
                        onClick={() => {
                          if (!evm.info || !walletAddress) return;
                          onMove({
                            sender: evm.info.address,
                            recipient: walletAddress,
                            balances: moves.balances,
                            start: item,
                          });
                        }}
                        icon={
                          <BadgedIcon
                            file={item.icon}
                            label={item.symbol}
                            badge={item.chainIcon}
                            badgeLabel={item.chainName}
                          />
                        }
                      />
                    ))}
                    {moves.transfers.map((item) => (
                      <div
                        className="flex h-[60px] w-full items-center rounded-2xl px-4 max-[795px]:px-2"
                        key={item.requestId}
                      >
                        <BadgedIcon
                          file={item.icon}
                          label={item.symbol}
                          badge={item.chainIcon}
                          badgeLabel={item.chainName}
                        />
                        <RowText
                          label={item.status}
                          value={`${item.amount} ${item.symbol}`}
                          badge={item.chainName}
                        />
                      </div>
                    ))}
                  </div>
                )}
                <div className="relative p-2">
                  <div
                    aria-hidden="true"
                    className={`pointer-events-none absolute left-[45px] top-[63px] w-0.5 bg-foreground/20 max-[795px]:left-[37px] ${
                      depositAsset ? "h-2.5" : "h-[18px]"
                    }`}
                  />
                  {depositAsset && (
                    <div
                      aria-hidden="true"
                      className="pointer-events-none absolute left-[45px] top-[123px] h-2.5 w-0.5 bg-foreground/20 max-[795px]:left-[37px]"
                    />
                  )}
                  <BridgeRow
                    label="Asset to deposit"
                    value={depositAsset?.symbol ?? "Select asset"}
                    placeholder={!depositAsset}
                    onClick={() => goTo("deposit-assets")}
                    icon={
                      depositAsset ? (
                        <AssetIcon
                          file={depositAsset.icon}
                          label={depositAsset.symbol}
                        />
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
                      value={network?.name ?? "Select chain"}
                      placeholder={!network}
                      onClick={() => goTo("chains")}
                      icon={
                        network ? (
                          <AssetIcon file={network.icon} label={network.name} />
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
                    <div className="flex h-[60px] w-full items-center rounded-2xl px-4 max-[795px]:px-2">
                      <BadgedIcon
                        file={RECEIVE.icon}
                        label={RECEIVE.symbol}
                        badge={RECEIVE.chainIcon}
                        badgeLabel={RECEIVE.chain}
                      />
                      <RowText
                        label="Asset to receive"
                        value={RECEIVE.symbol}
                        badge={RECEIVE.chain}
                      />
                      <span className="ml-3 flex size-6 shrink-0 items-center justify-center">
                        <InfoTooltip
                          iconClassName="size-6"
                          tone="dark"
                          text="The asset you’ll receive after bridging. Transferred assets will be automatically converted to this asset."
                        />
                      </span>
                    </div>
                  </div>
                </div>
                {resolved && (
                  <div className="flex flex-col gap-2 p-2 max-[795px]:px-4">
                    <Warning>
                      {narrow
                        ? `Send funds only via the ${network.shortName} network${
                            tronRoute ? "" : " and not from an exchange"
                          }.`
                        : `Send funds to this address only via the ${network.shortName} network.`}
                      {network.minUsd > 0 &&
                        ` The minimum amount is $${network.minUsd} in the selected asset.`}{" "}
                      Sending an unsupported token will result in permanent loss
                      of funds.
                    </Warning>
                    <AddressCard
                      address={bridgeAddress}
                      bridge
                      qrIcon={network.qrIcon}
                      depositSymbol={deposit.symbol}
                      emptyLabel={
                        routeError
                          ? "No deposit address yet"
                          : "Loading address…"
                      }
                      onCopy={handleCopy}
                    />
                    {(network.minUsd > 0 ||
                      deposit.feePercent ||
                      deposit.feeUsd ||
                      deposit.receiveUsdc) && (
                      <dl className="rounded-2xl bg-accent py-1 text-[13px] leading-4">
                        {network.minUsd > 0 && (
                          <div className="flex justify-between px-4 py-2.5">
                            <dt className="text-muted-foreground">
                              Minimum deposit amount
                            </dt>
                            <dd>{minDeposit(network.minUsd, deposit)}</dd>
                          </div>
                        )}
                        {(deposit.feePercent || deposit.feeUsd) && (
                          <div className="flex justify-between px-4 py-2.5">
                            <dt className="text-muted-foreground">Fee</dt>
                            <dd>
                              {deposit.feePercent
                                ? `${Number(deposit.feePercent)}%`
                                : `~$${Number(deposit.feeUsd).toFixed(2)}`}
                            </dd>
                          </div>
                        )}
                        {deposit.receiveUsdc && (
                          <div className="flex justify-between px-4 py-2.5">
                            <dt className="text-muted-foreground">
                              You receive about
                            </dt>
                            <dd>
                              {floorToCents(Number(deposit.receiveUsdc))}{" "}
                              {RECEIVE.symbol}
                            </dd>
                          </div>
                        )}
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
                  disabled={!(bridge ? bridgeAddress : walletAddress)}
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
            {(view === "chains" ? selectableNetworks : depositTokens).length >=
              SEARCH_MIN_ITEMS && (
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
                    onChange={(event) => setQuery(event.target.value)}
                    placeholder={
                      view === "chains" ? "Search chains" : "Search assets"
                    }
                    type="search"
                    value={query}
                  />
                </div>
              </div>
            )}
            <div className="min-h-0 flex-1 overflow-y-auto p-2">
              {networks.length === 0 &&
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
              {view === "chains" && selection
                ? filteredChains.map((item) => {
                    const token = tokenOn(item, selection.asset);
                    return (
                      <button
                        className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent max-[795px]:px-2"
                        key={item.chain}
                        onClick={() => {
                          setSelection({ ...selection, chain: item.chain });
                          setCopied(false);
                          goTo("receive");
                        }}
                        type="button"
                      >
                        <AssetIcon file={item.icon} label={item.name} />
                        <span className="ml-3 flex flex-1 flex-col gap-0.5">
                          <span className="font-medium text-[16px] leading-5 tracking-[-0.176px]">
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
                  })
                : filteredAssets.map((item) => (
                    <button
                      className="t-hover flex h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent max-[795px]:px-2"
                      key={item.asset}
                      onClick={() => {
                        setSelection({ asset: item.asset, chain: null });
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
              {networks.length > 0 &&
                (view === "chains" ? filteredChains : filteredAssets).length ===
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
