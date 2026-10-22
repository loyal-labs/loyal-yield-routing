import type { CSSProperties } from "react";

export function ThemedIcon({
  className,
  label,
  src,
}: {
  className?: string;
  label?: string;
  src: string;
}) {
  return (
    <span
      aria-hidden={label ? undefined : true}
      aria-label={label}
      className={`icon-themed ${className ?? ""}`}
      role={label ? "img" : undefined}
      style={{ "--icon": `url("${src}")` } as CSSProperties}
    />
  );
}
