"use client";

import { useRef } from "react";

import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "@/components/ui/dialog";
import { useEscapeOverlay } from "@/components/wallet-workspace/facelift/escape-owner";
import { TextSwap } from "@/components/wallet-workspace/facelift/text-swap";
import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";

import { useInviteLink } from "./use-invite-link";

/* eslint-disable @next/next/no-img-element -- Preserve the Figma SVGs’ fractional dimensions. */
const ASSET_BASE = "/wallet-workspace/facelift";

export function InviteDialog({
  isOpen,
  onClose,
}: {
  isOpen: boolean;
  onClose: () => void;
}) {
  const { link, error, copied, copy, retry } = useInviteLink(isOpen);
  const codeStart = (link?.lastIndexOf("/") ?? -1) + 1;
  const triggerRef = useRef<HTMLElement | null>(null);
  useEscapeOverlay(isOpen, onClose);

  return (
    <Dialog
      open={isOpen}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent
        className="flex max-h-[calc(100dvh-16px)] w-[400px] max-w-[calc(100%-16px)] flex-col gap-0 overflow-y-auto rounded-3xl border-0 bg-card p-0 text-foreground shadow-none sm:max-w-[400px] motion-reduce:animate-none"
        overlayClassName="bg-black/20 backdrop-blur-[4px] motion-reduce:animate-none"
        showCloseButton={false}
        onOpenAutoFocus={() => {
          triggerRef.current =
            document.activeElement instanceof HTMLElement
              ? document.activeElement
              : null;
        }}
        onCloseAutoFocus={(event) => {
          event.preventDefault();
          triggerRef.current?.focus({ preventScroll: true });
        }}
      >
        <DialogClose asChild>
          <button
            aria-label="Close invitation"
            className="t-hover absolute top-2 right-2 flex size-11 items-center justify-center rounded-3xl hover:bg-accent"
            type="button"
          >
            <ThemedIcon
              className="size-6 text-muted-foreground"
              src={`${ASSET_BASE}/invite-close.svg`}
            />
          </button>
        </DialogClose>
        <div className="flex flex-col items-center pt-16 pb-8">
          <div
            aria-hidden="true"
            className="relative h-32 w-48 shrink-0"
            data-invite-artwork
          >
            <div className="absolute top-[47.56px] left-[98.59px] flex size-[78.437px] items-center justify-center">
              <div className="relative size-[66.496px] rotate-[11.52deg] overflow-hidden rounded-[16.624px] bg-[#51b2f9]">
                <img
                  alt=""
                  className="absolute top-[15.05px] left-[8.312px] max-w-none"
                  src={`${ASSET_BASE}/invite-mascot-blue.svg`}
                />
              </div>
            </div>
            <div className="absolute top-[44.44px] left-[15.47px] flex size-[79.478px] items-center justify-center">
              <div className="relative size-[66.5px] rotate-[-12.68deg] overflow-hidden rounded-[16.625px] bg-[#4e49ff]">
                <img
                  alt=""
                  className="absolute top-[11.36px] left-[8.324px] max-w-none"
                  src={`${ASSET_BASE}/invite-mascot-purple.svg`}
                />
              </div>
            </div>
            <div className="absolute top-[6px] left-[60.15px] flex size-[72.449px] items-center justify-center">
              <div className="relative size-[66.5px] rotate-[5.39deg] overflow-hidden rounded-[16.625px] bg-[#ffd41b]">
                <img
                  alt=""
                  className="absolute top-[11.36px] left-[8.318px] max-w-none"
                  src={`${ASSET_BASE}/invite-mascot-yellow.svg`}
                />
              </div>
            </div>
          </div>
          <DialogTitle className="mt-5 max-w-[286px] text-center font-bold text-[28px] leading-8 tracking-[-0.28px] uppercase">
            Bring your friends to Loyal
          </DialogTitle>
          <DialogDescription className="mt-3 px-6 text-center text-[16px] leading-5 tracking-[-0.16px]">
            Share a link to invite someone
          </DialogDescription>
          <div className="mt-8 w-full px-6">
            <button
              aria-label="Copy invitation link"
              className="t-hover flex min-h-[60px] w-full items-center gap-2.5 rounded-2xl bg-accent px-4 py-2 text-left enabled:hover:bg-accent-selected disabled:cursor-wait"
              disabled={!link}
              onClick={copy}
              type="button"
            >
              <span
                className="min-w-0 flex-1 text-[16px] leading-5 tracking-[-0.16px] [overflow-wrap:anywhere]"
                data-invite-link
              >
                {link ? (
                  <>
                    {link.slice(0, codeStart)}
                    <wbr />
                    <span className="whitespace-nowrap">
                      {link.slice(codeStart)}
                    </span>
                  </>
                ) : error ? (
                  "Link unavailable"
                ) : (
                  "Loading invitation link…"
                )}
              </span>
              <ThemedIcon
                className="ml-3 size-6 shrink-0 text-tertiary"
                src={`${ASSET_BASE}/${
                  copied ? "icon-check.svg" : "invite-copy.svg"
                }`}
              />
            </button>
            {error ? (
              <p
                className="mt-3 text-[13px] text-destructive leading-4"
                role="alert"
              >
                {error}
              </p>
            ) : null}
          </div>
        </div>
        <div className="px-4 pt-2 pb-4">
          <button
            className="t-hover flex h-12 w-full items-center justify-center rounded-full bg-foreground px-5 font-medium text-[16px] text-background leading-5 enabled:hover:bg-foreground/90 disabled:opacity-40"
            disabled={!link && !error}
            onClick={!link && error ? retry : copy}
            type="button"
          >
            <span aria-live="polite">
              <TextSwap
                text={copied ? "Copied!" : !link && error ? "Try again" : "Copy invitation link"}
              />
            </span>
          </button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
