"use client";

import { useState } from "react";

import { copyTextToClipboard } from "@/components/wallet-workspace/facelift/copy-text";
import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";

const ASSET_BASE = "/wallet-workspace/facelift";

export function truncateMiddle(value: string): string {
  if (value.length <= 12) {
    return value;
  }
  return `${value.slice(0, 4)}…${value.slice(-4)}`;
}

export function RouteRow({
  icon,
  label,
  value,
  valueSuffix,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  valueSuffix?: string;
}) {
  return (
    <div className="flex h-[60px] w-full items-center rounded-2xl px-4">
      <div className="flex shrink-0 items-center py-2 pr-3">{icon}</div>
      <div className="flex min-w-0 flex-1 flex-col gap-0.5 py-2">
        <p className="text-[13px] leading-4 text-muted-foreground">{label}</p>
        <p className="truncate font-medium text-[16px] text-foreground leading-5">
          {value}
          {valueSuffix ? (
            <span className="text-tertiary"> · {valueSuffix}</span>
          ) : null}
        </p>
      </div>
    </div>
  );
}

export function SignatureCell({ signature }: { signature: string }) {
  const [copied, setCopied] = useState(false);

  return (
    <div className="flex w-full items-center">
      <div className="flex min-w-0 flex-1 flex-col gap-0.5 px-4 py-[11px]">
        <p className="text-[13px] leading-4 text-muted-foreground">Signature</p>
        <p className="truncate text-[16px] text-foreground leading-5 tracking-[-0.176px]">
          {truncateMiddle(signature)}
        </p>
      </div>
      <button
        aria-label="Copy transaction signature"
        className="t-hover flex shrink-0 items-center justify-center px-4"
        onClick={() => {
          void copyTextToClipboard(signature).then((didCopy) => {
            if (didCopy) {
              setCopied(true);
              window.setTimeout(() => setCopied(false), 1500);
            }
          });
        }}
        type="button"
      >
        <ThemedIcon
          className="size-6 text-tertiary"
          src={`${ASSET_BASE}/${copied ? "icon-check.svg" : "icon-copy.svg"}`}
        />
      </button>
    </div>
  );
}
