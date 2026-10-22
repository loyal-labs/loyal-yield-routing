"use client";

import { Fragment, useEffect, useRef, useState } from "react";

import { LOYAL_TOKEN_MINT } from "@/lib/market/loyal-token-ticker.shared";

const ASSET_BASE = "/wallet-workspace/facelift";

const SLIDE_GAP_PX = 8;

export const MOBILE_BANNER_SLOT_CLASS = "relative h-24 w-full shrink-0";

const AUTOPLAY_INTERVAL_MS = 4000;

const BUTTON_COLOR_CLASSES = {
  black: "bg-black hover:bg-[#171717]",
  red: "bg-[#f9363c] hover:bg-[#e42f35]",
} as const;

type HomeBanner = {
  art: string;
  buttonColor: keyof typeof BUTTON_COLOR_CLASSES;
  buttonLabel: string;
  bg: string;
  href: string | null;
  key: string;
  title: string;
};

const BANNERS: HomeBanner[] = [
  {
    art: `${ASSET_BASE}/banner-x-art.svg`,
    bg: `${ASSET_BASE}/banner-x-bg.png`,
    buttonColor: "black",
    buttonLabel: "Follow",
    href: "https://x.com/loyal_hq",
    key: "x",
    title: "Follow\nLoyal on X",
  },
  {
    art: `${ASSET_BASE}/banner-jupiter-art.svg`,
    bg: `${ASSET_BASE}/banner-dark-bg.png`,
    buttonColor: "red",
    buttonLabel: "Go to Jupiter",
    href: `https://jup.ag/tokens/${LOYAL_TOKEN_MINT}`,
    key: "jupiter",
    title: "Buy $LOYAL on Jupiter",
  },
  {
    art: `${ASSET_BASE}/banner-telegram-art.svg`,
    bg: `${ASSET_BASE}/banner-telegram-bg.png`,
    buttonColor: "black",
    buttonLabel: "Join",
    href: "https://t.me/loyal_tgchat",
    key: "telegram",
    title: "Join Telegram Community",
  },
  {
    art: `${ASSET_BASE}/banner-autodeposit-art.svg`,
    bg: `${ASSET_BASE}/banner-dark-bg.png`,
    buttonColor: "red",
    buttonLabel: "Set up Autodeposit",
    href: null,
    key: "autodeposit",
    title: "Start earning the moment your money arrives",
  },
];

export function BannerTitleChars({ title }: { title: string }) {
  let charIndex = 0;
  return (
    <>
      {title.split("\n").map((line, lineIndex) => (
        <Fragment key={line}>
          {lineIndex > 0 ? <br /> : null}
          {line.split(" ").map((word, wordIndex) => (
            <Fragment key={`${word}-${String(wordIndex)}`}>
              {wordIndex > 0 ? " " : null}
              <span className="inline-block">
                {Array.from(word).map((char, charOffset) => (
                  <span
                    className="t-banner-char"
                    key={`${char}-${String(charOffset)}`}
                    style={{ ["--banner-char-i" as never]: charIndex++ }}
                  >
                    {char}
                  </span>
                ))}
              </span>
            </Fragment>
          ))}
        </Fragment>
      ))}
    </>
  );
}

export function FirstDepositBanner() {
  return (
    <div className="flex h-full w-full items-center justify-end overflow-clip rounded-3xl bg-black/[0.04] dark:bg-white/[0.06]">
      <p className="min-w-0 flex-1 self-start p-4 font-medium text-[16px] text-foreground leading-[18px]">
        Make your first
        <br />
        deposit to get started
      </p>
      <div className="relative aspect-square h-full shrink-0 overflow-clip">
        {/* eslint-disable-next-line @next/next/no-img-element */}
        <img
          alt=""
          aria-hidden="true"
          className="absolute right-[-12%] bottom-[-8%] w-[110%] max-w-none"
          src={`${ASSET_BASE}/banner-mascot.svg`}
        />
      </div>
    </div>
  );
}

export function WalletHomeBanners({
  dotsBelow = false,
  onSetUpAutodeposit,
}: {
  dotsBelow?: boolean;
  onSetUpAutodeposit: () => void;
}) {
  const scrollerRef = useRef<HTMLDivElement>(null);
  const [activeIndex, setActiveIndex] = useState(0);
  const isPointerDownRef = useRef(false);

  // is-shown only lands after the first paint so the initial slide's
  // entrance actually transitions instead of rendering pre-revealed.
  const [hasMounted, setHasMounted] = useState(false);
  useEffect(() => {
    setHasMounted(true);
  }, []);

  useEffect(() => {
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      return;
    }
    const timer = setInterval(() => {
      const scroller = scrollerRef.current;
      if (!scroller || isPointerDownRef.current) {
        return;
      }
      const next = (activeIndex + 1) % BANNERS.length;
      scroller.scrollTo({
        behavior: "smooth",
        left: next * (scroller.clientWidth + SLIDE_GAP_PX),
      });
    }, AUTOPLAY_INTERVAL_MS);
    return () => clearInterval(timer);
  }, [activeIndex]);

  const handleScroll = () => {
    const scroller = scrollerRef.current;
    if (!scroller) {
      return;
    }
    const index = Math.round(
      scroller.scrollLeft / (scroller.clientWidth + SLIDE_GAP_PX)
    );
    setActiveIndex(Math.min(Math.max(index, 0), BANNERS.length - 1));
  };

  const buttonClassName = (banner: HomeBanner) =>
    `t-hover flex min-w-16 items-center justify-center rounded-full px-4 py-2.5 text-center font-medium text-[13px] text-white leading-4 max-[795px]:hidden ${
      BUTTON_COLOR_CLASSES[banner.buttonColor]
    }`;

  return (
    <div className="relative col-span-2 h-full">
      <div
        aria-hidden="true"
        className={`-translate-x-1/2 absolute left-1/2 flex h-4 items-center gap-2 ${
          dotsBelow ? "-bottom-5" : "-top-5"
        }`}
      >
        {BANNERS.map((banner, index) => (
          <span
            className={`size-2 rounded-full transition-colors duration-200 ${
              index === activeIndex ? "bg-primary" : "bg-primary/10"
            }`}
            key={banner.key}
          />
        ))}
      </div>
      <div
        className="scrollbar-hide flex h-full snap-x snap-mandatory overflow-x-auto overscroll-x-contain"
        onPointerCancel={() => {
          isPointerDownRef.current = false;
        }}
        onPointerDown={() => {
          isPointerDownRef.current = true;
        }}
        onPointerUp={() => {
          isPointerDownRef.current = false;
        }}
        onScroll={handleScroll}
        ref={scrollerRef}
        style={{ gap: SLIDE_GAP_PX }}
      >
        {BANNERS.map((banner, index) => (
          <section
            className={`t-stagger t-home-banner relative flex h-full w-full shrink-0 snap-start overflow-clip rounded-3xl ${
              hasMounted && index === activeIndex ? "is-shown" : ""
            }`}
            key={banner.key}
          >
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img
              alt=""
              aria-hidden="true"
              className="pointer-events-none absolute inset-0 size-full object-cover"
              src={banner.bg}
            />
            {banner.href !== null ? (
              <a
                aria-label={banner.title}
                className="absolute inset-0 z-10 min-[796px]:hidden"
                href={banner.href}
                rel="noreferrer"
                target="_blank"
              />
            ) : (
              <button
                aria-label={banner.title}
                className="absolute inset-0 z-10 min-[796px]:hidden"
                onClick={onSetUpAutodeposit}
                type="button"
              />
            )}
            <div className="relative flex min-w-0 flex-1 flex-col items-start justify-between p-4">
              <h3 className="max-w-[240px] font-semibold text-[20px] text-white leading-5 max-[795px]:text-[16px] max-[795px]:leading-[18px]">
                <BannerTitleChars title={banner.title} />
              </h3>
              {/* The pill hides on mobile via its own class — the wrapper
                  can't carry it (.t-stagger-line's unlayered display:block
                  outranks the layered hidden utility). */}
              <div
                className="t-stagger-line"
                style={{ transitionDelay: "calc(var(--stagger-stagger) * 3)" }}
              >
                {banner.href !== null ? (
                  <a
                    className={buttonClassName(banner)}
                    href={banner.href}
                    rel="noreferrer"
                    target="_blank"
                  >
                    {banner.buttonLabel}
                  </a>
                ) : (
                  <button
                    className={buttonClassName(banner)}
                    onClick={onSetUpAutodeposit}
                    type="button"
                  >
                    {banner.buttonLabel}
                  </button>
                )}
              </div>
            </div>
            <div className="relative aspect-square h-full shrink-0">
              <div
                className="t-stagger-line absolute inset-0"
                style={{ transitionDelay: "calc(var(--stagger-stagger) * 2)" }}
              >
                {/* eslint-disable-next-line @next/next/no-img-element */}
                <img
                  alt=""
                  aria-hidden="true"
                  className="size-full"
                  src={banner.art}
                />
              </div>
            </div>
          </section>
        ))}
      </div>
    </div>
  );
}
