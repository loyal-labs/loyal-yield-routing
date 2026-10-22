"use client";

import { Moon, Sun } from "lucide-react";
import { useEffect } from "react";

import { SIDEBAR_LINKS } from "@/components/wallet-workspace/facelift/sidebar";
import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";
import { TitledSheet } from "@/components/wallet-workspace/facelift/titled-sheet";
import { useSessionDisconnect } from "@/components/wallet-workspace/facelift/use-session-disconnect";
import { useSignInModal } from "@/contexts/sign-in-modal-context";
import { useTheme } from "@/hooks/use-theme";

const ASSET_BASE = "/wallet-workspace/facelift";

const LINK_BY_ICON = Object.fromEntries(
  SIDEBAR_LINKS.map((link) => [link.icon, link])
);

const LINK_GROUPS: { href: string; icon: string; label: string }[][] = [
  [
    { ...LINK_BY_ICON["icon-docs.svg"], label: "See documentation" },
    { ...LINK_BY_ICON["icon-bug.svg"], label: "Report a bug" },
    { ...LINK_BY_ICON["icon-support.svg"], label: "Get support" },
  ],
  [
    { ...LINK_BY_ICON["icon-x-social.svg"], label: "Follow Loyal on X" },
    { ...LINK_BY_ICON["icon-globe.svg"], label: "Visit askloyal.com" },
  ],
];

// Figma 5462:75016 (disconnected) / 5462:74914 (connected) — the mobile
// settings sheet behind the header gear: theme stepper, the sidebar's link
// destinations, and connect/disconnect.
export function SettingsSheet({
  isOpen,
  onClose,
}: {
  isOpen: boolean;
  onClose: () => void;
}) {
  const { isDark, toggleTheme } = useTheme();
  const { open: openSignIn } = useSignInModal();
  const { canDisconnect, disconnectAll } = useSessionDisconnect();

  useEffect(() => {
    if (!isOpen) {
      return;
    }
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        onClose();
      }
    };
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [isOpen, onClose]);

  return (
    <TitledSheet
      closeLabel="Close settings"
      isOpen={isOpen}
      onClose={onClose}
      title="Settings"
    >
      <div className="flex w-full flex-col gap-2 px-4 pb-4">
        <button
          className="t-hover flex h-[60px] w-full items-center gap-3 rounded-[20px] bg-accent px-4 hover:bg-accent-selected"
          onClick={(event) => {
            const rect = event.currentTarget.getBoundingClientRect();
            toggleTheme({
              x: rect.left + rect.width / 2,
              y: rect.top + rect.height / 2,
            });
          }}
          type="button"
        >
          {isDark ? (
            <Moon className="size-6 shrink-0 text-tertiary" />
          ) : (
            <Sun className="size-6 shrink-0 text-tertiary" />
          )}
          <span className="min-w-0 flex-1 text-left text-[16px] text-foreground leading-5">
            Appearance
          </span>
          <span className="flex shrink-0 items-center gap-1">
            <span className="text-[16px] leading-5 text-muted-foreground">
              {isDark ? "Dark" : "Light"}
            </span>
            <ThemedIcon
              className="size-6 text-tertiary"
              src={`${ASSET_BASE}/icon-arrow-top-bottom.svg`}
            />
          </span>
        </button>
        {LINK_GROUPS.map((group) => (
          <div
            className="flex w-full flex-col overflow-clip rounded-[20px] bg-accent"
            key={group[0].href}
          >
            {group.map((link) => (
              <a
                className="t-hover flex h-[60px] w-full items-center gap-3 px-4 hover:bg-accent-selected"
                href={link.href}
                key={link.href}
                rel="noreferrer"
                target="_blank"
              >
                <ThemedIcon
                  className="size-6 shrink-0 text-tertiary"
                  src={`${ASSET_BASE}/${link.icon}`}
                />
                <span className="min-w-0 flex-1 text-[16px] text-foreground leading-5">
                  {link.label}
                </span>
                <ThemedIcon
                  className="size-6 shrink-0 text-tertiary"
                  src={`${ASSET_BASE}/icon-chevron-right.svg`}
                />
              </a>
            ))}
          </div>
        ))}
        {canDisconnect ? (
          <button
            className="t-hover flex h-14 w-full items-center justify-center gap-3 rounded-full bg-destructive/[0.08] px-4 hover:bg-destructive/[0.14]"
            onClick={() => {
              disconnectAll();
              onClose();
            }}
            type="button"
          >
            <ThemedIcon
              className="size-6 shrink-0 text-destructive"
              src={`${ASSET_BASE}/icon-logout.svg`}
            />
            <span className="font-medium text-[16px] text-destructive leading-5">
              Disconnect wallet
            </span>
          </button>
        ) : (
          <button
            className="t-hover flex h-14 w-full items-center justify-center gap-3 rounded-full bg-foreground px-4 hover:bg-foreground/90"
            onClick={() => {
              onClose();
              openSignIn();
            }}
            type="button"
          >
            <ThemedIcon
              className="size-6 shrink-0 text-background"
              src={`${ASSET_BASE}/icon-wallet-fill.svg`}
            />
            <span className="font-medium text-[16px] text-background leading-5">
              Connect wallet
            </span>
          </button>
        )}
      </div>
    </TitledSheet>
  );
}
