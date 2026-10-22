"use client";

import {
  createContext,
  useContext,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import {
  PopDigits,
  type PopDigitsSegment,
} from "@/components/wallet-workspace/facelift/pop-digits";

type BalanceVisibility = {
  isBalanceHidden: boolean;
  toggleBalanceHidden: () => void;
};

const BalanceVisibilityContext = createContext<BalanceVisibility>({
  isBalanceHidden: false,
  toggleBalanceHidden: () => {},
});

export function BalanceVisibilityProvider({
  children,
}: {
  children: ReactNode;
}) {
  const [isBalanceHidden, setIsBalanceHidden] = useState(false);
  const value = useMemo(
    () => ({
      isBalanceHidden,
      toggleBalanceHidden: () => setIsBalanceHidden((current) => !current),
    }),
    [isBalanceHidden]
  );
  return (
    <BalanceVisibilityContext.Provider value={value}>
      {children}
    </BalanceVisibilityContext.Provider>
  );
}

export function useBalanceVisibility() {
  return useContext(BalanceVisibilityContext);
}

// Hidden numbers scramble into ASCII symbols instead of blurring (the
// motion.dev scramble-text look). The resting mask hashes character POSITION
// and a per-element seed — never the digit it replaces — so it looks random
// yet can't be reversed and stays identical across live value refreshes.
const SCRAMBLE_SYMBOLS = "!#$%&()*+/:;<=>?@[]^_{|}~abcdefghjkmnpqrstuvwxyz";
const SCRAMBLE_TICK_MS = 30;

function scrambleSymbolAt(index: number, seed: number): string {
  let hash = Math.imul(index + 1, 2654435761) ^ seed;
  hash ^= hash >>> 13;
  hash = Math.imul(hash, 1597334677);
  hash ^= hash >>> 16;
  return SCRAMBLE_SYMBOLS[(hash >>> 0) % SCRAMBLE_SYMBOLS.length];
}

function toSeedNumber(seed: number | string): number {
  if (typeof seed === "number") {
    return seed;
  }
  let hash = 0;
  for (let index = 0; index < seed.length; index += 1) {
    hash = Math.imul(hash ^ seed.charCodeAt(index), 2654435761);
  }
  return hash;
}

export function maskBalanceText(
  text: string,
  seed: number | string = 0
): string {
  const seedNumber = toSeedNumber(seed);
  return Array.from(text, (char, index) =>
    char === " " ? char : scrambleSymbolAt(index, seedNumber)
  ).join("");
}

function useScramble(text: string, isHidden: boolean) {
  const seedRef = useRef(Math.floor(Math.random() * 0xffffffff));
  const target = isHidden ? maskBalanceText(text, seedRef.current) : text;
  const [churn, setChurn] = useState<string | null>(null);
  const targetRef = useRef(target);
  targetRef.current = target;
  const hiddenRef = useRef(isHidden);

  useLayoutEffect(() => {
    if (hiddenRef.current === isHidden) {
      return;
    }
    hiddenRef.current = isHidden;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      return;
    }
    let locked = 0;
    let intervalId = 0;
    const tick = () => {
      const current = targetRef.current;
      if (locked >= current.length) {
        window.clearInterval(intervalId);
        setChurn(null);
        return;
      }
      setChurn(
        current.slice(0, locked) +
          Array.from(current.slice(locked), (char) =>
            char === " "
              ? char
              : SCRAMBLE_SYMBOLS[
                  Math.floor(Math.random() * SCRAMBLE_SYMBOLS.length)
                ]
          ).join("")
      );
      locked += Math.max(1, Math.ceil(current.length / 10));
    };
    tick();
    intervalId = window.setInterval(tick, SCRAMBLE_TICK_MS);
    return () => {
      window.clearInterval(intervalId);
      setChurn(null);
    };
  }, [isHidden]);

  return { display: churn ?? target, isSettled: churn === null };
}

export function ScrambleText({
  isHidden,
  text,
}: {
  isHidden: boolean;
  text: string;
}) {
  const { display } = useScramble(text, isHidden);
  return <>{display}</>;
}

export function ScrambledPopDigits({
  isHidden,
  popOnChange = true,
  segments,
}: {
  isHidden: boolean;
  popOnChange?: boolean;
  segments: PopDigitsSegment[];
}) {
  const { display, isSettled } = useScramble(
    segments.map((segment) => segment.text).join(""),
    isHidden
  );
  let offset = 0;
  const scrambledSegments = segments.map((segment) => {
    const text = display.slice(offset, offset + segment.text.length);
    offset += segment.text.length;
    return { ...segment, text };
  });
  return (
    <PopDigits
      popOnChange={popOnChange && !isHidden && isSettled}
      segments={scrambledSegments}
    />
  );
}

// The reused ForecastChart hides values via the old front's filter ids
// (url(#rs-pixelate-*)); the facelift defines those ids as BLUR filters so the
// whole shell hides numbers consistently. The old views inline their own
// pixelating defs, so this does not affect them.
export function HiddenBalanceFilterDefs() {
  return (
    <svg
      aria-hidden="true"
      style={{ height: 0, position: "absolute", width: 0 }}
    >
      <defs>
        <filter id="rs-pixelate-lg">
          <feGaussianBlur stdDeviation="10" />
        </filter>
        <filter id="rs-pixelate-sm">
          <feGaussianBlur stdDeviation="8" />
        </filter>
      </defs>
    </svg>
  );
}
