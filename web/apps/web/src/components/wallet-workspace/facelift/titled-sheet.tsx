"use client";

import type { ReactNode } from "react";

import { SheetReveal } from "@/components/wallet-workspace/facelift/sheet-reveal";
import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";

const ASSET_BASE = "/wallet-workspace/facelift";

export function TitledSheet({
  children,
  closeLabel,
  isOpen,
  onClose,
  title,
}: {
  children: ReactNode;
  closeLabel: string;
  isOpen: boolean;
  onClose: () => void;
  title: string;
}) {
  return (
    <SheetReveal
      isOpen={isOpen}
      onClose={onClose}
      scrimClassName="fixed inset-0 z-50 flex items-center justify-center bg-black/20 p-2 backdrop-blur-[4px] max-[795px]:items-end max-[795px]:bg-white/60 max-[795px]:p-0"
      sheetClassName="flex w-[400px] min-w-0 flex-col overflow-clip rounded-3xl bg-card max-[795px]:w-full max-[795px]:rounded-b-none max-[795px]:shadow-[0px_-10px_40px_-10px_rgba(0,0,0,0.2)]"
    >
      <header className="flex w-full items-center p-2">
        <h2 className="min-w-0 flex-1 truncate py-2.5 pl-4 font-semibold text-[20px] text-foreground leading-6">
          {title}
        </h2>
        <button
          aria-label={closeLabel}
          className="t-hover flex size-11 shrink-0 items-center justify-center rounded-3xl hover:bg-accent"
          onClick={onClose}
          type="button"
        >
          <ThemedIcon
            className="size-6 text-muted-foreground"
            src={`${ASSET_BASE}/icon-cross.svg`}
          />
        </button>
      </header>
      {children}
    </SheetReveal>
  );
}
