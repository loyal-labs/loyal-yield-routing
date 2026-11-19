"use client";

import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";
import { InvitedFriends } from "./invited-friends";
import { useInviteLink } from "./use-invite-link";

const ASSET_BASE = "/wallet-workspace/facelift";

/* eslint-disable @next/next/no-img-element -- Use the original Loyal invite illustrations at their designed dimensions. */
function InviteArtwork() {
  return (
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
  );
}

export function InvitePage({ onBack }: { onBack: () => void }) {
  const { link, friends, error, copied, copy, retry } = useInviteLink(true);

  return (
    <main aria-labelledby="invite-title" className="h-full min-h-0 flex-1 overflow-y-auto bg-card text-foreground">
      <div className="mx-auto w-full max-w-[900px] px-4 pt-4 pb-12 sm:px-8 sm:pt-6 lg:px-12">
        <button
          className="t-hover mb-4 inline-flex min-h-11 items-center gap-1.5 rounded-full pr-3 pl-2 text-sm font-medium hover:bg-accent"
          onClick={onBack}
          type="button"
        >
          <ThemedIcon className="size-5" src={`${ASSET_BASE}/icon-arrow-left.svg`} />
          Back
        </button>

        <header className="rounded-3xl bg-violet-50 px-5 py-6 sm:px-8 sm:py-8 dark:bg-violet-950/35">
          <div className="relative grid grid-cols-[minmax(0,1fr)_72px] items-center gap-x-2 sm:grid-cols-[minmax(0,1fr)_152px] sm:gap-x-5">
            <h1 id="invite-title" className="max-w-lg text-balance text-[32px] leading-[1.06] font-bold tracking-[-0.045em] sm:text-[42px]">
              Invite friends. Earn 0.3% a year.
            </h1>
            <div className="relative h-14 w-[72px] sm:row-span-2 sm:h-[105px] sm:w-[152px]" aria-hidden="true">
              <div className="absolute top-1/2 right-0 origin-right -translate-y-1/2 scale-[0.4] sm:scale-[0.83]">
                <InviteArtwork />
              </div>
            </div>
            <p className="col-span-2 mt-3 max-w-xl text-base leading-6 sm:col-span-1">
              You earn rewards on the amount each friend keeps in Earn Max after the entry fee.
            </p>
          </div>
          <p className="mt-4 max-w-xl border-l-2 border-violet-400 pl-3 text-[15px] leading-6">
            If your friends keep <strong>$100,000</strong> deposited for a full year, you earn <strong>$300</strong>.
          </p>
        </header>

        <section aria-labelledby="invite-link-title" className="mt-7 sm:mt-8">
          <h2 id="invite-link-title" className="text-lg leading-6 font-semibold tracking-tight">Your invite link</h2>
          <div className="mt-3 flex flex-col gap-2 min-[540px]:flex-row">
            <div className="flex min-h-12 min-w-0 flex-1 items-center rounded-xl border border-border bg-accent px-4">
              <span className="select-all break-all text-[15px]" data-invite-link>
                {link ?? (error ? "Link unavailable" : "Loading invitation link…")}
              </span>
            </div>
            <button
              className="t-hover inline-flex min-h-12 items-center justify-center gap-2 rounded-full bg-foreground px-5 text-[15px] font-medium text-background enabled:hover:bg-foreground/90 disabled:opacity-40"
              disabled={!link && !error}
              onClick={!link && error ? retry : copy}
              type="button"
            >
              <ThemedIcon className="size-[18px]" src={`${ASSET_BASE}/${copied ? "icon-check.svg" : "invite-copy.svg"}`} />
              <span aria-live="polite">{copied ? "Copied!" : !link && error ? "Try again" : "Copy invite link"}</span>
            </button>
          </div>
          {error && <p className="mt-2 text-sm text-destructive" role="alert">{error}</p>}
          <p className="mt-3 max-w-2xl text-sm leading-5 text-foreground/70">
            You’re paid in Earn Max shares as Loyal collects performance fees from each friend’s deposit. Anything still owed carries over for that friend.
          </p>
        </section>

        {friends && <InvitedFriends friends={friends} />}
      </div>
    </main>
  );
}
