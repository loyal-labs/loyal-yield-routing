"use client";

import type { SmartAccountPolicySnapshot } from "@loyal-labs/smart-account-vaults";
import Image from "next/image";
import { useWallet } from "@solana/wallet-adapter-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { PrivyAccountPanel } from "@/components/auth/privy-account-panel";
import { useAuthSession } from "@/contexts/auth-session-context";
import { usePublicEnv } from "@/contexts/public-env-context";
import { useSignInModal } from "@/contexts/sign-in-modal-context";
import { useCherryRuntime } from "@/features/cherry/client/runtime-context";
import { useEscapeOverlay } from "./escape-owner";
import { useSessionDisconnect } from "./use-session-disconnect";
import { useTheme } from "@/hooks/use-theme";
import { copyTextToClipboard } from "./copy-text";
import { InfoTooltip } from "./info-tooltip";
import { SheetReveal } from "./sheet-reveal";
import { SIDEBAR_LINKS } from "./sidebar";
import { ThemedIcon } from "./themed-icon";
import type { EarnPositionData } from "./use-earn-position-data";
import type { PolicyUpdates } from "./use-policy-updates";

const BASE = "/wallet-workspace/facelift";
const smallButton =
  "t-hover shrink-0 rounded-full bg-accent px-4 py-2.5 font-medium text-[13px] leading-4 enabled:hover:bg-accent-selected disabled:opacity-40";

function SectionTitle({ children }: { children: ReactNode }) {
  return (
    <h3 className="flex items-center gap-2 px-4 pt-4 pb-2 font-semibold text-[16px] leading-5 tracking-[-0.176px]">
      {children}
    </h3>
  );
}

function SettingsRow({
  icon,
  title,
  subtitle,
  children,
  onClick,
  disabled = false,
}: {
  icon: string;
  title: string;
  subtitle?: string;
  children?: ReactNode;
  onClick?: () => void;
  disabled?: boolean;
}) {
  return (
    <button
      className="t-hover flex min-h-[60px] w-full items-center rounded-2xl px-4 text-left enabled:hover:bg-accent disabled:cursor-default disabled:opacity-40"
      disabled={disabled}
      onClick={onClick}
      type="button"
    >
      {icon === "settings/shield.svg" ? (
        <Image
          alt=""
          className="mr-4 shrink-0"
          src={`${BASE}/${icon}`}
          width={24}
          height={24}
        />
      ) : (
        <ThemedIcon
          className="mr-4 size-6 shrink-0 text-tertiary"
          src={`${BASE}/${icon}`}
        />
      )}
      <span className="min-w-0 flex-1 py-[11px]">
        <span className="block font-medium text-[16px] leading-5 tracking-[-0.176px]">
          {title}
        </span>
        {subtitle && (
          <span className="mt-0.5 block text-[13px] leading-4 text-destructive">
            {subtitle}
          </span>
        )}
      </span>
      {children}
    </button>
  );
}

function Chevron() {
  return (
    <ThemedIcon
      className="ml-1.5 size-6 shrink-0 text-tertiary"
      src={`${BASE}/icon-chevron-right.svg`}
    />
  );
}
function policyTitle(
  policy: SmartAccountPolicySnapshot,
  data: EarnPositionData
) {
  const earn = data.smartAccountData.earnPolicy;
  if (policy.address === earn?.account) return "Yield routing";
  if (policy.address === earn?.setupPolicy?.account) return "Lending setup";
  if (policy.address === data.autodepositConfig?.policyAccount)
    return "Autodeposit";
  return policy.state === "ProgramInteraction"
    ? "Program interaction"
    : policy.state === "SpendingLimit"
    ? "Spending limit"
    : policy.state;
}

export function SettingsSheet({
  isOpen,
  onClose,
  initialView = "settings",
  data,
  updates,
}: {
  isOpen: boolean;
  onClose: () => void;
  initialView?: "settings" | "policies";
  data: EarnPositionData;
  updates: PolicyUpdates;
}) {
  const { isDark, toggleTheme } = useTheme();
  const { isAuthenticated, user } = useAuthSession();
  const { wallet, publicKey } = useWallet();
  const { open: openSignIn } = useSignInModal();
  const { privyAppId, solanaEnv } = usePublicEnv();
  const cherry = useCherryRuntime();
  const { canDisconnect, disconnectAll } = useSessionDisconnect();
  useEscapeOverlay(isOpen, onClose);
  const [view, setView] = useState<"settings" | "policies">(initialView);
  const [selected, setSelected] = useState<SmartAccountPolicySnapshot | null>(
    null
  );
  const [copied, setCopied] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  const address = user?.walletAddress ?? publicKey?.toBase58();
  const walletIcons: Record<string, string> = {
    phantom: "wallet-phantom.svg",
    solflare: "wallet-solflare.svg",
    jupiter: "wallet-jupiter.png",
    zerion: "wallet-zerion.svg",
  };
  const walletIcon =
    publicKey?.toBase58() === address
      ? walletIcons[wallet?.adapter.name.toLowerCase() ?? ""]
      : undefined;
  const count = updates.earnCount + updates.earnMaxCount;
  const policies = data.smartAccountData.overview?.policies ?? [];
  const policyError = data.smartAccountData.scopedErrors.policies;

  useEffect(() => {
    if (!isOpen) return;
    setView(initialView);
    setSelected(null);
    const previous =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    return () => {
      previous?.focus();
    };
  }, [isOpen, initialView, address]);
  useEffect(() => {
    if (!isOpen) return;
    const frame = requestAnimationFrame(() =>
      // Keep focus from scrolling the overlay while its panel is offscreen.
      panelRef.current?.querySelector<HTMLButtonElement>("button")?.focus({ preventScroll: true })
    );
    return () => cancelAnimationFrame(frame);
  }, [isOpen, view, selected]);
  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 1500);
    return () => clearTimeout(timer);
  }, [copied]);
  useEffect(() => {
    if (!isOpen) return;
    const keydown = (event: KeyboardEvent) => {
      // Privy's nested dialogs own their keyboard handling while open.
      if (!panelRef.current?.contains(event.target as Node)) return;
      if (event.key !== "Tab") return;
      const elements = [
        ...panelRef.current.querySelectorAll<HTMLElement>(
          'button:not(:disabled), a[href], [tabindex="0"]'
        ),
      ];
      const first = elements[0];
      const last = elements.at(-1);
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last?.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first?.focus();
      }
    };
    window.addEventListener("keydown", keydown, true);
    return () => window.removeEventListener("keydown", keydown, true);
  }, [isOpen, onClose]);

  const policyRow = (
    <SettingsRow
      icon="settings/shield.svg"
      title="Onchain Policies"
      subtitle={
        count
          ? `${count} ${count === 1 ? "update" : "updates"} required`
          : undefined
      }
      onClick={() => setView("policies")}
    >
      {count > 0 && (
        <ThemedIcon
          className="size-6 text-destructive"
          src={`${BASE}/icon-warning-triangle.svg`}
        />
      )}
      <Chevron />
    </SettingsRow>
  );
  const required = selected?.address === updates.requiredAccount;
  const title = selected
    ? policyTitle(selected, data)
    : view === "policies"
    ? "Onchain Policies"
    : "Settings";
  return (
    <SheetReveal
      label={title}
      isOpen={isOpen}
      onClose={onClose}
      scrimClassName="fixed inset-0 z-40 flex overflow-hidden items-start justify-end bg-black/20 p-2 max-[795px]:items-end max-[795px]:p-0"
      sheetClassName="t-settings-panel flex h-full max-h-[calc(100dvh-16px)] w-[400px] min-w-0 flex-col overflow-hidden rounded-3xl bg-card max-[795px]:max-h-[95dvh] max-[795px]:w-full max-[795px]:rounded-b-none"
    >
      <div className="flex min-h-0 flex-1 flex-col" ref={panelRef}>
        <header className="flex shrink-0 items-center p-2">
          {(selected || view === "policies") && (
            <button
              aria-label={selected ? "Back to policies" : "Back to settings"}
              className="t-hover mr-3 flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
              onClick={() =>
                selected ? setSelected(null) : setView("settings")
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
              view === "settings" ? "pl-4" : ""
            }`}
          >
            {title}
          </h2>
          {view === "policies" && !selected && (
            <InfoTooltip text="Onchain policies define the programs, signers, and limits your smart account permits. Updates require your wallet approval." />
          )}
          <button
            aria-label="Close settings"
            className="t-hover ml-3 flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
            onClick={onClose}
            type="button"
          >
            <ThemedIcon
              className="size-6 text-muted-foreground"
              src={`${BASE}/icon-cross.svg`}
            />
          </button>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-4">
          {view === "settings" ? (
            <>
              <div className="flex min-h-[60px] items-center px-4">
                {/* eslint-disable-next-line @next/next/no-img-element */}
                <img
                  alt=""
                  className="mr-3 size-11 rounded-[11px]"
                  src={`${BASE}/${walletIcon ?? "wallet-dog.svg"}`}
                />
                <div className="min-w-0 flex-1 py-[11px]">
                  <p className="text-[13px] leading-4 text-muted-foreground">
                    {address ? "Connected wallet" : "Wallet"}
                  </p>
                  <button
                    className="mt-0.5 flex items-center gap-1 font-medium text-[16px] leading-5"
                    disabled={!address}
                    onClick={() =>
                      address &&
                      void copyTextToClipboard(address).then(setCopied)
                    }
                    type="button"
                    aria-label={
                      copied ? "Address copied" : "Copy wallet address"
                    }
                  >
                    {address
                      ? `${address.slice(0, 4)}…${address.slice(-4)}`
                      : "Not connected"}
                    <ThemedIcon
                      className="size-5 text-tertiary"
                      src={`${BASE}/${
                        copied ? "icon-check.svg" : "icon-copy.svg"
                      }`}
                    />
                  </button>
                </div>
                {cherry.mode === "standalone" && (
                  <button
                    className={`${smallButton} ml-3 ${
                      canDisconnect
                        ? "bg-destructive/[0.14]"
                        : ""
                    }`}
                    onClick={() => {
                      if (canDisconnect) {
                        disconnectAll();
                        onClose();
                      } else {
                        onClose();
                        openSignIn();
                      }
                    }}
                    type="button"
                  >
                    {canDisconnect ? "Sign out" : "Sign in"}
                  </button>
                )}
              </div>
              {isAuthenticated && (
                <>
                  <SectionTitle>Account</SectionTitle>
                  {privyAppId && cherry.mode === "standalone" ? (
                    <PrivyAccountPanel policyRow={policyRow} />
                  ) : (
                    policyRow
                  )}
                </>
              )}
              <SectionTitle>Preferences</SectionTitle>
              <SettingsRow
                icon="settings/sun.svg"
                title="Appearance"
                onClick={() => toggleTheme()}
              >
                <span className={`${smallButton} ml-3 flex items-center gap-2`}>
                  <ThemedIcon
                    className="size-4"
                    src={`${BASE}/settings/${isDark ? "sun" : "moon"}.svg`}
                  />
                  {isDark ? "Use light theme" : "Use dark theme"}
                </span>
              </SettingsRow>
              <SettingsRow
                icon="settings/eye-off.svg"
                title="Hide unsupported assets"
                disabled
              >
                <span
                  aria-label="Unavailable until a supported-asset list exists"
                  className="ml-3 flex h-8 w-[52px] items-center rounded-full bg-accent-selected px-1"
                >
                  <span className="size-6 rounded-full bg-white" />
                </span>
              </SettingsRow>
              <SettingsRow icon="icon-globe.svg" title="Language" disabled>
                <span className="text-[16px] text-muted-foreground">
                  English
                </span>
              </SettingsRow>
              {[
                { title: "Help", icons: ["icon-bug.svg", "icon-support.svg"] },
                {
                  title: "About",
                  icons: [
                    "icon-docs.svg",
                    "icon-x-social.svg",
                    "icon-globe.svg",
                  ],
                },
              ].map((group) => (
                <section key={group.title}>
                  <SectionTitle>{group.title}</SectionTitle>
                  {group.icons.map((icon) => {
                    const link = SIDEBAR_LINKS.find(
                      (item) => item.icon === icon
                    )!;
                    return (
                      <a
                        key={icon}
                        className="t-hover flex min-h-[60px] items-center rounded-2xl px-4 hover:bg-accent"
                        href={link.href}
                        target="_blank"
                        rel="noreferrer"
                      >
                        <ThemedIcon
                          className="mr-4 size-6 shrink-0 text-tertiary"
                          src={`${BASE}/${icon}`}
                        />
                        <span className="flex-1 font-medium text-[16px] leading-5 tracking-[-0.176px]">
                          {icon === "icon-docs.svg"
                            ? "See documentation"
                            : icon === "icon-support.svg"
                            ? "Get support"
                            : link.label}
                        </span>
                        <Chevron />
                      </a>
                    );
                  })}
                </section>
              ))}
            </>
          ) : selected ? (
            <div className="space-y-5 px-4 py-4">
              <p className="text-[16px] leading-6 text-muted-foreground">
                {required
                  ? "Update the Earn routing permissions to support Token-2022 stablecoins. This creates the current routing and lending setup policies; your existing policies and funds are preserved. Review both transactions in your wallet."
                  : "These permissions are enforced by your smart account on Solana."}
              </p>
              <dl className="space-y-4 text-[13px] leading-5">
                {[
                  ["Policy account", selected.address],
                  ["Type", selected.state],
                  ["Vault", String(selected.accountIndex ?? "—")],
                  ["Approval threshold", String(selected.threshold)],
                  ["Time lock", `${selected.timeLock} seconds`],
                  ["Policy seed", selected.seed],
                  [
                    "Signers",
                    selected.signers.map((s) => s.address).join(", ") || "None",
                  ],
                ].map(([label, value]) => (
                  <div key={label}>
                    <dt className="text-muted-foreground">{label}</dt>
                    <dd className="break-all">{value}</dd>
                  </div>
                ))}
              </dl>
            </div>
          ) : (
            <>
              {data.smartAccountData.isPoliciesLoading && (
                <p className="px-4 py-3 text-muted-foreground" role="status">
                  Loading policies…
                </p>
              )}
              {policyError && (
                <div
                  className="px-4 py-3 text-[13px] text-destructive"
                  role="alert"
                >
                  Could not load policies.{" "}
                  <button
                    className="underline"
                    onClick={() =>
                      void data.smartAccountData.refreshGroups({
                        groups: ["policies"],
                      })
                    }
                    type="button"
                  >
                    Retry
                  </button>
                </div>
              )}
              {[
                { name: "Earn", index: 1, icon: "earn-icon.svg" },
                { name: "Earn MAX", index: 2, icon: "earn-max-icon.svg" },
              ].map((group) => (
                <section key={group.index}>
                  <SectionTitle>
                    {/* eslint-disable-next-line @next/next/no-img-element */}
                    <Image
                      alt=""
                      width={20}
                      height={20}
                      className="size-5"
                      src={`${BASE}/${group.icon}`}
                    />
                    {group.name}
                  </SectionTitle>
                  {policies
                    .filter((p) => p.accountIndex === group.index)
                    .map((policy) => (
                      <button
                        key={policy.address}
                        className="t-hover flex min-h-[60px] w-full items-center rounded-2xl px-4 text-left hover:bg-accent"
                        onClick={() => setSelected(policy)}
                        type="button"
                      >
                        <span className="min-w-0 flex-1 py-[11px]">
                          <span className="block font-medium text-[16px] leading-5 tracking-[-0.176px]">
                            {policyTitle(policy, data)}
                          </span>
                          <span
                            className={`mt-0.5 block text-[13px] leading-4 ${
                              policy.address === updates.requiredAccount
                                ? "text-destructive"
                                : "text-muted-foreground"
                            }`}
                          >
                            {policy.address === updates.requiredAccount
                              ? "Updates required"
                              : `${policy.address.slice(
                                  0,
                                  4
                                )}…${policy.address.slice(-4)}`}
                          </span>
                        </span>
                        {policy.address === updates.requiredAccount && (
                          <span className={smallButton}>Update</span>
                        )}
                        <Chevron />
                      </button>
                    ))}
                  {!data.smartAccountData.isPoliciesLoading &&
                    !policyError &&
                    !policies.some((p) => p.accountIndex === group.index) && (
                      <p className="px-4 py-3 text-[13px] leading-4 text-muted-foreground">
                        No onchain policies.
                      </p>
                    )}
                </section>
              ))}
            </>
          )}
          {view === "policies" && updates.error && (
            <div
              className="px-4 py-3 text-[13px] leading-5 text-destructive"
              role="alert"
            >
              {updates.error}{" "}
              <button
                className="underline"
                onClick={() => void updates.refresh()}
                type="button"
              >
                Check again
              </button>
            </div>
          )}
        </div>
        {selected && (
          <footer className="flex shrink-0 gap-2 px-4 pt-2 pb-4">
            <a
              className="t-hover flex h-12 min-w-0 flex-1 items-center justify-center rounded-full bg-accent px-5 font-medium text-[16px] hover:bg-accent-selected"
              href={`https://explorer.solana.com/address/${selected.address}${
                solanaEnv === "devnet" ? "?cluster=devnet" : ""
              }`}
              target="_blank"
              rel="noreferrer"
            >
              View on explorer
            </a>
            {required && (
              <button
                className="t-hover h-12 min-w-0 flex-1 rounded-full bg-foreground px-5 font-medium text-[16px] text-background disabled:opacity-40"
                disabled={
                  updates.isUpdating || data.smartAccountData.isActionPending
                }
                onClick={() => void updates.update()}
                type="button"
              >
                {updates.isUpdating ? "Updating…" : "Update policy"}
              </button>
            )}
          </footer>
        )}
      </div>
    </SheetReveal>
  );
}
