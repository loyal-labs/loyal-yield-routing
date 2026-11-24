import type { ReactNode } from "react";

export const chartTimestamp = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
  timeZone: "UTC",
});

export function EarnChartHeadline({ children, label }: { children: ReactNode; label: string }) {
  return (
    <div className="flex shrink-0 flex-col gap-0.5 py-2">
      <p className="truncate text-[16px] leading-5 text-muted-foreground">{label}</p>
      <div className="truncate text-[40px] font-semibold leading-[48px] tracking-[-0.44px] text-foreground">
        {children}
      </div>
    </div>
  );
}

export function EarnChartDates({ labels }: { labels: string[] }) {
  return (
    <div className="flex shrink-0 justify-between gap-1 pt-2 text-[13px] leading-4 text-muted-foreground">
      {labels.map((label, index) => (
        <span
          className="hidden whitespace-nowrap first:inline last:inline @min-[640px]:inline"
          key={index}
        >
          {label}
        </span>
      ))}
    </div>
  );
}
