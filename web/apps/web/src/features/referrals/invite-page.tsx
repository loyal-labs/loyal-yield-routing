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
  const { link, rewardsActive, friends, error, copied, copy, retry } = useInviteLink(true);
  const codeStart = (link?.lastIndexOf("/") ?? -1) + 1;
  const isLoading = !link && !error;
  const inviteTitle = link ? "Invite friends" : "Your invite";
  const inviteDescription = link ? "Share your link to invite someone to Loyal." : error;

  return (
    <main aria-labelledby="invite-title" className="h-full min-h-0 flex-1 overflow-y-auto bg-card text-foreground">
      <div className="@container mx-auto w-full max-w-[900px] px-4 pt-4 pb-12 sm:px-8 sm:pt-6 lg:px-12">
        <button
          className="t-hover -ml-2 mb-4 inline-flex min-h-11 items-center gap-1.5 rounded-full px-2.5 text-sm font-medium hover:bg-accent"
          onClick={onBack}
          type="button"
        >
          <ThemedIcon className="size-5" src={`${ASSET_BASE}/icon-arrow-left.svg`} />
          Back
        </button>

        {isLoading ? (
          <div aria-busy="true" role="status">
            <span className="sr-only" id="invite-title">Loading invitation</span>
            <div aria-hidden="true" className="space-y-6">
              <div className="rounded-3xl bg-[#51031b] bg-cover bg-center px-5 py-7 @xl:px-8 @xl:py-9" style={{ backgroundImage: `url(${ASSET_BASE}/banner-dark-bg.png)` }}>
                <div className="h-[clamp(2rem,12cqi,3rem)] w-2/5 rounded-lg bg-white/10 motion-safe:animate-pulse @xl:h-[clamp(3rem,9cqi,4.5rem)]" />
                <div className="mt-3 h-12 w-3/4 max-w-[30rem] rounded-lg bg-white/10 motion-safe:animate-pulse @xl:h-14" />
              </div>
              <div className="h-32 rounded-3xl bg-secondary @xl:h-[88px]" />
            </div>
          </div>
        ) : (
          <StaggerReveal>
            <header
              className="relative isolate overflow-hidden rounded-3xl bg-[#51031b] bg-cover bg-center px-5 py-7 text-white @xl:px-8 @xl:py-9"
              style={{ backgroundImage: `url(${ASSET_BASE}/banner-dark-bg.png)` }}
            >
              <StaggerLine index={0}>
                <h1
                  aria-describedby="invite-example-condition"
                  className="pr-16 text-[clamp(2rem,12cqi,3rem)] leading-none font-bold tracking-[-0.045em] whitespace-nowrap @xl:pr-48 @xl:text-[clamp(3rem,9cqi,4.5rem)]"
                  id="invite-title"
                >
                  {rewardsActive ? <>Earn <span className="text-[#ffd41b]">$300</span></> : inviteTitle}
                </h1>
              </StaggerLine>
              <StaggerLine index={1}>
                <p id="invite-example-condition" role={link ? undefined : "alert"} className="mt-3 max-w-[30rem] text-pretty text-lg leading-6 @xl:mr-48 @xl:text-[22px] @xl:leading-7">
                  {rewardsActive ? <>when your friends keep <strong>$100,000</strong> in Earn MAX for a full year.</> : inviteDescription}
                </p>
              </StaggerLine>
              <div className="pointer-events-none absolute top-6 right-4 origin-top-right scale-[0.42] @xl:top-1/2 @xl:right-6 @xl:-translate-y-1/2 @xl:scale-100" aria-hidden="true">
                <StaggerLine index={2}>
                  <InviteArtwork />
                </StaggerLine>
              </div>
            </header>

            <section aria-labelledby="invite-link-title" className="mt-6">
              <div className="grid items-center gap-4 rounded-3xl bg-secondary p-4 @xl:grid-cols-[minmax(0,1fr)_12rem] @xl:px-6 @xl:py-5">
                <div className="min-w-0">
                  <StaggerLine index={3}>
                    <h2 id="invite-link-title" className="text-sm leading-5 text-foreground/70">Your invite link</h2>
                  </StaggerLine>
                  <StaggerLine index={4}>
                    <p className="mt-1 w-fit max-w-full select-all text-base leading-5 font-medium [overflow-wrap:anywhere]" data-invite-link>
                      {link ? (
                        <>
                          {link.slice(0, codeStart)}
                          <wbr />
                          <span className="whitespace-nowrap">{link.slice(codeStart)}</span>
                        </>
                      ) : "Link unavailable"}
                    </p>
                  </StaggerLine>
                </div>
                <StaggerLine index={5}>
                  <button
                    className="t-hover flex h-12 w-full items-center justify-center rounded-full bg-foreground px-4 text-base leading-5 font-medium whitespace-nowrap text-background enabled:hover:bg-foreground/90 disabled:opacity-40"
                    onClick={link ? copy : retry}
                    type="button"
                  >
                    <span aria-live="polite">
                      <TextSwap text={copied ? "Copied!" : link ? "Copy invitation link" : "Try again"} />
                    </span>
                  </button>
                </StaggerLine>
              </div>
              {link && error && <p className="mt-2 text-sm text-destructive" role="alert">{error}</p>}
              {rewardsActive && <StaggerLine index={6}>
                <p className="mt-3 max-w-2xl text-sm leading-5 text-foreground/70">
                  You earn 0.3% a year on each friend’s deposits after the entry fee. Their performance fees fund your payouts in Earn MAX shares. Unpaid rewards carry over for that friend.
                </p>
              </StaggerLine>}
            </section>
          </StaggerReveal>
        )}

        {friends && (
          <StaggerReveal>
            <StaggerLine index={7}>
              <InvitedFriends friends={friends} rewardsActive={rewardsActive} />
            </StaggerLine>
          </StaggerReveal>
        )}
      </div>
    </main>
  );
}
