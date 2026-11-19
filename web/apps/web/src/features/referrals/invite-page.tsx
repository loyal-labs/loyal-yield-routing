"use client";

import { StaggerLine, StaggerReveal } from "@/components/wallet-workspace/facelift/stagger-reveal";
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
      <div className="@container mx-auto w-full max-w-[900px] px-4 pt-4 pb-12 sm:px-8 sm:pt-6 lg:px-12">
        <StaggerReveal>
          <StaggerLine index={0}>
            <div className="mb-4 flex items-center gap-2">
              <button
                aria-label="Back"
                className="t-hover -ml-2 flex size-11 items-center justify-center rounded-full hover:bg-accent"
                onClick={onBack}
                type="button"
              >
                <ThemedIcon className="size-6" src={`${ASSET_BASE}/icon-arrow-left.svg`} />
              </button>
              <span className="text-xl leading-6 font-semibold">Invite friends</span>
            </div>
          </StaggerLine>

          <StaggerLine index={1}>
            <header
              className="relative isolate overflow-hidden rounded-3xl bg-[#51031b] bg-cover bg-center px-5 py-7 text-white @xl:px-8 @xl:py-9"
              style={{ backgroundImage: `url(${ASSET_BASE}/banner-dark-bg.png)` }}
            >
              <div className="absolute top-6 right-4 origin-top-right scale-[0.42] @xl:top-1/2 @xl:right-6 @xl:-translate-y-1/2 @xl:scale-100" aria-hidden="true">
                <InviteArtwork />
              </div>
              <h1 aria-describedby="invite-example-condition" id="invite-title" className="relative flex flex-wrap items-baseline gap-x-2 pr-16 font-bold tracking-tight @xl:gap-x-3 @xl:pr-48">
                <span className="text-[32px] leading-none @xl:text-[44px]">Earn</span>
                <span className="text-[clamp(3rem,17cqi,3.75rem)] leading-none tracking-[-0.055em] text-[#ffd41b] @xl:text-[88px]">$300</span>
              </h1>
              <p id="invite-example-condition" className="relative mt-3 max-w-[30rem] text-pretty text-lg leading-6 @xl:mr-48 @xl:text-[22px] @xl:leading-7">
                when your friends keep <strong>$100,000</strong> in Earn MAX for a full year.
              </p>
            </header>
          </StaggerLine>

          <StaggerLine index={2}>
            <section aria-labelledby="invite-link-title" className="mt-6">
              <div className="grid items-center gap-4 rounded-3xl bg-secondary p-4 @xl:grid-cols-[minmax(0,1fr)_12rem] @xl:px-6 @xl:py-5">
                <div className="min-w-0">
                  <h2 id="invite-link-title" className="text-sm leading-5 text-foreground/70">Your invite link</h2>
                  <p className="mt-1 w-fit max-w-full select-all text-base leading-5 font-medium [overflow-wrap:anywhere]" data-invite-link>
                    {link ? (
                      <>
                        {link.slice(0, codeStart)}
                        <wbr />
                        <span className="whitespace-nowrap">{link.slice(codeStart)}</span>
                      </>
                    ) : error ? "Link unavailable" : "Loading invitation link…"}
                  </p>
                </div>
                <button
                  className="t-hover flex h-12 w-full items-center justify-center rounded-full bg-foreground px-4 text-base leading-5 font-medium whitespace-nowrap text-background enabled:hover:bg-foreground/90 disabled:opacity-40"
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
                You earn 0.3% a year on each friend’s deposits after the entry fee. Their performance fees fund your payouts in Earn MAX shares. Unpaid rewards carry over for that friend.
              </p>
            </section>
          </StaggerLine>
        </StaggerReveal>

        {friends && (
          <StaggerReveal>
            <StaggerLine index={3}>
              <InvitedFriends friends={friends} />
            </StaggerLine>
          </StaggerReveal>
        )}
      </div>
    </main>
  );
}
