"use client";

import { useLayoutEffect, useRef, type ReactNode } from "react";

export function StaggerReveal({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  const groupRef = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    const el = groupRef.current;
    if (!el) {
      return;
    }
    el.classList.remove("is-shown");
    void el.offsetHeight;
    el.classList.add("is-shown");
  }, []);

  return (
    <div className={`t-stagger ${className ?? ""}`} ref={groupRef}>
      {children}
    </div>
  );
}

export function StaggerLine({
  children,
  index,
}: {
  children: ReactNode;
  index: number;
}) {
  return (
    <div
      className="t-stagger-line"
      style={{ transitionDelay: `calc(var(--stagger-stagger) * ${index})` }}
    >
      {children}
    </div>
  );
}
