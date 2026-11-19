"use client";

import { TextSwap } from "@/components/wallet-workspace/facelift/text-swap";
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
  const codeStart = (link?.lastIndexOf("/") ?? -1) + 1;

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

        <section aria-labelledby="invite-title" className="rounded-3xl bg-secondary p-5 sm:p-8">
          <header className="grid grid-cols-[minmax(0,1fr)_64px] items-center gap-x-2 sm:grid-cols-[minmax(0,1fr)_144px] sm:gap-x-6">
            <h1 id="invite-title" className="text-[28px] leading-8 font-bold tracking-[-0.28px] sm:text-[36px] sm:leading-10">
              Invite friends. <span className="block">Earn 0.3% a year.</span>
            </h1>
            <div className="relative h-14 sm:row-span-2 sm:h-24" aria-hidden="true">
              <div className="absolute top-1/2 right-0 origin-right -translate-y-1/2 scale-[0.36] sm:scale-75">
                <InviteArtwork />
              </div>
            </div>
            <div className="col-span-2 mt-3 max-w-xl space-y-2 text-base leading-[22px] tracking-[-0.16px] sm:col-span-1">
              <p>Your reward is based on what each friend keeps in Earn MAX after the entry fee.</p>
              <p>If your friends keep $100,000 deposited for a full year, <strong>you earn $300.</strong></p>
            </div>
          </header>

          <h2 className="mt-6 text-base leading-5 font-semibold">Your invite link</h2>
          <div className="mt-2 flex flex-col gap-2 min-[540px]:flex-row">
            <button
              aria-describedby="invite-url"
              aria-label="Copy invitation URL"
              className="t-hover flex min-h-12 min-w-0 flex-1 items-center gap-3 rounded-2xl bg-card px-4 py-3 text-left enabled:hover:bg-accent-selected disabled:cursor-default"
              disabled={!link}
              onClick={copy}
              type="button"
            >
              <span id="invite-url" className="min-w-0 flex-1 text-base leading-5 tracking-[-0.16px] [overflow-wrap:anywhere]" data-invite-link>
                {link ? (
                  <>
                    {link.slice(0, codeStart)}
                    <wbr />
                    <span className="whitespace-nowrap">{link.slice(codeStart)}</span>
                  </>
                ) : error ? "Link unavailable" : "Loading invitation link…"}
              </span>
              <ThemedIcon className="size-6 shrink-0 text-muted-foreground" src={`${ASSET_BASE}/${copied ? "icon-check.svg" : "invite-copy.svg"}`} />
            </button>
            <button
              className="t-hover flex min-h-12 items-center justify-center rounded-full bg-foreground px-5 text-base leading-5 font-medium text-background enabled:hover:bg-foreground/90 disabled:opacity-40 min-[540px]:w-48"
              disabled={!link && !error}
              onClick={!link && error ? retry : copy}
              type="button"
            >
              <span aria-live="polite">
                <TextSwap text={copied ? "Copied!" : !link && error ? "Try again" : "Copy invitation link"} />
              </span>
            </button>
          </div>
          {error && <p className="mt-2 text-sm text-destructive" role="alert">{error}</p>}
          <p className="mt-3 max-w-2xl text-sm leading-5 text-foreground/70">
            Rewards are paid in Earn MAX shares from each friend’s performance fees. Any unpaid amount carries over for that friend.
          </p>
        </section>

        {friends && <InvitedFriends friends={friends} />}
      </div>
    </main>
  );
}
