"use client";

import { useEscapeOverlay } from "@/components/wallet-workspace/facelift/escape-owner";
import type { LucideIcon } from "lucide-react";

import { SheetReveal } from "@/components/wallet-workspace/facelift/sheet-reveal";
import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";

const ASSET_BASE = "/wallet-workspace/facelift";

export type EarnModalStep = {
  description?: string;
  icon: LucideIcon;
  title: string;
};

export function EarnModal({
  buttonLabel,
  description,
  icon: Icon,
  isOpen,
  onAction,
  onClose,
  steps,
  title,
  withMascot = true,
}: {
  buttonLabel: string;
  description?: string;
  icon?: LucideIcon;
  isOpen: boolean;
  onAction: () => void;
  onClose: () => void;
  steps?: EarnModalStep[];
  title: string;
  withMascot?: boolean;
}) {
  useEscapeOverlay(isOpen, onClose);

  return (
    <SheetReveal
      isOpen={isOpen}
      onClose={onClose}
      scrimClassName={`fixed inset-0 z-50 flex flex-col bg-black/20 backdrop-blur-[4px] max-[795px]:items-stretch max-[795px]:justify-end max-[795px]:bg-white/60 max-[795px]:p-0 max-[795px]:pt-8 ${
        withMascot
          ? "items-end justify-end p-2"
          : "items-center justify-center p-4"
      }`}
      sheetClassName={`flex max-w-full flex-col overflow-clip rounded-3xl bg-card max-[795px]:w-full max-[795px]:rounded-b-none ${
        withMascot ? "w-[724px]" : "w-[400px]"
      }`}
    >
      <div className="relative flex w-full items-start">
        <div className="flex w-[400px] flex-col max-[795px]:w-full">
          <div className="flex min-h-[300px] w-full flex-col p-2 max-[795px]:min-h-0">
            <div className="flex w-full flex-col gap-6 px-4 pt-4 pb-2">
              {Icon ? (
                <span className="flex size-16 items-center justify-center rounded-[14.667px] bg-primary">
                  <Icon aria-hidden="true" className="size-11 text-white" />
                </span>
              ) : null}
              <div className="flex w-full flex-col gap-3 pr-4">
                <h2 className="font-semibold text-[36px] text-foreground leading-10 tracking-[-0.72px]">
                  {title}
                </h2>
                {description ? (
                  <p className="text-[16px] leading-5 text-muted-foreground">
                    {description}
                  </p>
                ) : null}
              </div>
            </div>
            {steps && steps.length > 0 ? (
              <div className="flex w-full flex-col pb-2">
                {steps.map((step) => (
                  <div className="flex w-full items-start px-4" key={step.title}>
                    <span className="flex items-start py-[9px] pr-3">
                      <step.icon
                        aria-hidden="true"
                        className="size-6 text-muted-foreground"
                      />
                    </span>
                    <span className="flex min-w-0 flex-1 flex-col gap-0.5 py-[11px]">
                      <span className="font-medium text-[16px] text-foreground leading-5">
                        {step.title}
                      </span>
                      {step.description ? (
                        <span className="text-[13px] leading-4 text-muted-foreground">
                          {step.description}
                        </span>
                      ) : null}
                    </span>
                  </div>
                ))}
              </div>
            ) : null}
          </div>
          <div className="w-full px-4 pt-2 pb-4">
            <button
              className="t-hover flex h-12 w-full items-center justify-center rounded-full bg-foreground font-medium text-[16px] text-background leading-5 hover:-translate-y-0.5 hover:bg-foreground/90 active:translate-y-0"
              onClick={onAction}
              type="button"
            >
              {buttonLabel}
            </button>
          </div>
        </div>

        {withMascot ? (
          <div className="relative min-h-full w-[324px] shrink-0 self-stretch max-[795px]:hidden">
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img
              alt=""
              aria-hidden="true"
              className="pointer-events-none absolute right-[-64px] bottom-[-22px] w-[369px] max-w-none"
              src={`${ASSET_BASE}/modal-mascot.svg`}
            />
          </div>
        ) : null}

        <div className="absolute top-2 right-2">
          <button
            aria-label="Close"
            className="t-hover flex size-11 items-center justify-center rounded-3xl hover:bg-accent"
            onClick={onClose}
            type="button"
          >
            <ThemedIcon
              className="size-6 text-muted-foreground"
              src={`${ASSET_BASE}/icon-cross.svg`}
            />
          </button>
        </div>
      </div>
    </SheetReveal>
  );
}
