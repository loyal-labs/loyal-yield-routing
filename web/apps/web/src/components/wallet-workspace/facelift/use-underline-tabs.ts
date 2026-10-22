"use client";

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  type RefObject,
} from "react";

export function useUnderlineTabs(activeKey: string): {
  tabBarRef: RefObject<HTMLDivElement | null>;
  underlineRef: RefObject<HTMLSpanElement | null>;
} {
  const tabBarRef = useRef<HTMLDivElement>(null);
  const underlineRef = useRef<HTMLSpanElement>(null);
  const hasPaintedTabs = useRef(false);
  const moveUnderlineToActiveTab = useCallback((animate: boolean) => {
    const bar = tabBarRef.current;
    const underline = underlineRef.current;
    const active = bar?.querySelector<HTMLButtonElement>(
      '[aria-selected="true"]'
    );
    if (!(bar && underline && active)) {
      return;
    }
    const write = () => {
      underline.style.transform = `translateX(${active.offsetLeft + 12}px)`;
      underline.style.width = `${active.offsetWidth - 24}px`;
    };
    if (animate) {
      write();
      return;
    }
    const previousTransition = underline.style.transition;
    underline.style.transition = "none";
    write();
    void underline.offsetWidth;
    underline.style.transition = previousTransition;
  }, []);

  useLayoutEffect(() => {
    moveUnderlineToActiveTab(hasPaintedTabs.current);
    hasPaintedTabs.current = true;
  }, [activeKey, moveUnderlineToActiveTab]);

  useEffect(() => {
    const handleResize = () => moveUnderlineToActiveTab(false);
    window.addEventListener("resize", handleResize);
    return () => window.removeEventListener("resize", handleResize);
  }, [moveUnderlineToActiveTab]);

  return { tabBarRef, underlineRef };
}
