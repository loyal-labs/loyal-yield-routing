"use client";

import {
  useEffect,
  useSyncExternalStore,
  type ReactNode,
  type RefObject,
} from "react";

import { BridgeWalletIcon, shortBridgeAddress } from "./bridge-recipient-pane";
import { PaneReveal } from "./pane-transitions";
import { InlineSheetReveal, SheetReveal } from "./sheet-reveal";
import { TextSwap } from "./text-swap";
import { TokenIconSwap } from "./token-icon-swap";
import { ThemedIcon } from "./themed-icon";

const BASE = "/wallet-workspace/facelift";
const DESKTOP_QUERY = "(min-width: 1204px)";

function subscribeDesktop(onChange: () => void) {
  const media = window.matchMedia(DESKTOP_QUERY);
  media.addEventListener("change", onChange);
  return () => media.removeEventListener("change", onChange);
}

export function useBridgeDesktop() {
  return useSyncExternalStore(
    subscribeDesktop,
    () => window.matchMedia(DESKTOP_QUERY).matches,
    () => true
  );
}

export function BridgeIcon({
  file,
  badge,
  size = 44,
}: {
  file: string;
  badge?: string;
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
      {badge && (
        // eslint-disable-next-line @next/next/no-img-element
        <img
          alt=""
          className="absolute right-0 bottom-0 size-4 rounded-full border-2 border-card"
          height={16}
          src={`${BASE}/receive/${badge}`}
          width={16}
        />
      )}
    </span>
  );
}

export function TokenPill({
  icon,
  badge,
  symbol,
  network,
  label,
  onClick,
}: {
  icon: string;
  badge: string;
  symbol: string;
  network: string;
  label: string;
  onClick?: () => void;
}) {
  const body = (
    <>
      <BridgeIcon badge={badge} file={icon} size={36} />
      <span className="flex flex-col pl-2">
        <span className="text-[16px] font-medium leading-5">
          <TextSwap text={symbol} />
        </span>
        <span className="text-[13px] leading-4 text-muted-foreground">
          <TextSwap text={network} />
        </span>
      </span>
    </>
  );
  if (!onClick)
    return (
      <span
        aria-label={label}
        className="ml-3 flex shrink-0 items-center rounded-full bg-accent py-1 pr-4 pl-1 text-left"
      >
        {body}
      </span>
    );
  return (
    <button
      aria-label={label}
      className="t-hover ml-3 flex shrink-0 items-center rounded-full bg-accent py-1 pr-2.5 pl-1 text-left hover:bg-accent-active"
      onClick={onClick}
      type="button"
    >
      {body}
      <ThemedIcon
        className="size-6 text-tertiary"
        src={`${BASE}/icon-chevron-right.svg`}
      />
    </button>
  );
}

export const BRIDGE_AMOUNT_TEXT =
  "min-w-0 flex-1 text-[40px] font-semibold leading-[48px] tracking-[-1px] max-[480px]:text-[32px]";

export function BridgeDivider() {
  return (
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
  );
}

export function BridgeQuoteRows({
  label,
  rows,
}: {
  label: string;
  rows: [string, string][];
}) {
  return (
    <dl
      className="mx-2 mb-2 rounded-2xl bg-accent px-4 py-1 text-[13px] leading-4"
      aria-label={label}
    >
      {rows.map(([term, value]) => (
        <div className="flex justify-between gap-3 py-2.5" key={term}>
          <dt className="text-muted-foreground">{term}</dt>
          <dd className="text-right">{value}</dd>
        </div>
      ))}
    </dl>
  );
}

export function BridgeScreen({
  onBack,
  children,
}: {
  onBack: () => void;
  children: ReactNode;
}) {
  return (
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
          <ThemedIcon className="size-6" src={`${BASE}/icon-arrow-left.svg`} />
        </button>
        <h1 className="px-4 py-2.5 text-[20px] font-semibold leading-6">
          Bridge
        </h1>
      </header>
      {children}
    </section>
  );
}

export function BridgeCta({
  tone,
  label,
  onClick,
}: {
  tone: "ready" | "blocked" | "busy";
  label: string;
  onClick: () => void;
}) {
  return (
    <button
      className={`t-hover flex h-12 w-full items-center justify-center rounded-full px-3 text-[16px] font-medium leading-5 ${
        tone === "ready"
          ? "bg-foreground text-background hover:opacity-90"
          : tone === "busy"
          ? "bg-foreground text-background opacity-60"
          : "bg-destructive/[0.14] text-destructive"
      }`}
      disabled={tone === "busy"}
      onClick={onClick}
      type="button"
    >
      <TextSwap text={label} />
    </button>
  );
}

export function useBridgePickerFocus(
  panelRef: RefObject<HTMLElement | null>,
  open: boolean,
  desktop: boolean
) {
  useEffect(() => {
    if (!open) return;
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
  }, [panelRef, open, desktop]);
}

export function BridgePickerHeader({
  title,
  onClose,
  onBack,
}: {
  title: string;
  onClose: () => void;
  onBack?: () => void;
}) {
  return (
    <header className="flex shrink-0 items-center p-2">
      {onBack && (
        <button
          aria-label="Back to receive assets"
          className="t-hover flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
          onClick={onBack}
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
          onBack ? "pl-3" : "pl-4"
        }`}
      >
        {title}
      </h2>
      <button
        aria-label="Close bridge selector"
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
  );
}

export function BridgePickerRow({
  icon,
  badge,
  name,
  detail,
  value,
  onClick,
}: {
  icon: string;
  badge?: string;
  name: string;
  detail?: string;
  value?: [string, string];
  onClick: () => void;
}) {
  return (
    <button
      className="t-hover flex min-h-[60px] w-full items-center gap-3 rounded-2xl px-4 text-left hover:bg-accent focus-visible:bg-accent"
      onClick={onClick}
      type="button"
    >
      <BridgeIcon badge={badge} file={icon} />
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="text-[16px] font-medium leading-5">{name}</span>
        {detail && (
          <span className="text-[13px] leading-4 text-muted-foreground">
            {detail}
          </span>
        )}
      </span>
      {value && (
        <span className="flex flex-col items-end gap-0.5">
          <span className="text-[16px] font-medium leading-5">{value[0]}</span>
          <span className="text-[13px] leading-4 text-muted-foreground">
            {value[1]}
          </span>
        </span>
      )}
    </button>
  );
}

export function BridgePickerHost({
  desktop,
  open,
  title,
  view,
  onClose,
  children,
}: {
  desktop: boolean;
  open: boolean;
  title: string;
  view: string;
  onClose: () => void;
  children: ReactNode;
}) {
  return desktop ? (
    <aside className="h-full w-[400px] shrink-0">
      <InlineSheetReveal
        className="h-full overflow-clip rounded-3xl bg-card"
        isOpen={open}
      >
        <PaneReveal key={view}>{children}</PaneReveal>
      </InlineSheetReveal>
    </aside>
  ) : (
    <SheetReveal
      isOpen={open}
      label={title}
      onClose={onClose}
      scrimClassName="fixed inset-0 z-50 flex bg-black/20 p-2 backdrop-blur-[4px] max-[795px]:bg-white/60 max-[795px]:p-0 max-[795px]:pt-8"
      sheetClassName="ml-auto flex h-full w-[392px] min-w-0 max-w-full flex-col overflow-clip rounded-3xl bg-card max-[795px]:w-full max-[795px]:rounded-b-none max-[795px]:shadow-[0px_-10px_40px_-10px_rgba(0,0,0,0.2)]"
    >
      <PaneReveal key={view}>{children}</PaneReveal>
    </SheetReveal>
  );
}

export function BridgeRecipientRow({
  network,
  recipient,
  active = false,
  onOpen,
  onClear,
}: {
  network: string;
  recipient: string;
  active?: boolean;
  onOpen?: () => void;
  onClear?: () => void;
}) {
  const body = (
    <>
      <BridgeWalletIcon />
      <span className="flex min-w-0 flex-1 flex-col gap-1">
        <span className="text-[13px] leading-4 text-muted-foreground">
          {network} recipient
        </span>
        <span
          className={`truncate text-[20px] font-medium leading-6 ${
            recipient ? "" : "text-tertiary"
          }`}
        >
          {recipient
            ? shortBridgeAddress(recipient)
            : `Enter ${network} address`}
        </span>
      </span>
    </>
  );
  return (
    <div className="px-2 pb-2">
      <div
        className={`flex items-center rounded-2xl pr-2 ${
          active ? "bg-accent" : ""
        }`}
      >
        {onOpen ? (
          <button
            className="t-hover flex min-h-[60px] min-w-0 flex-1 items-center gap-3 rounded-2xl px-4 text-left hover:bg-accent"
            onClick={onOpen}
            type="button"
          >
            {body}
            {!recipient && (
              <ThemedIcon
                className="size-6 shrink-0 text-muted-foreground"
                src={`${BASE}/icon-chevron-right.svg`}
              />
            )}
          </button>
        ) : (
          <div className="flex min-h-[60px] min-w-0 flex-1 items-center gap-3 rounded-2xl px-4 text-left">
            {body}
          </div>
        )}
        {recipient && onClear && (
          <button
            aria-label="Remove recipient"
            className="t-hover flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
            onClick={onClear}
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
  );
}
