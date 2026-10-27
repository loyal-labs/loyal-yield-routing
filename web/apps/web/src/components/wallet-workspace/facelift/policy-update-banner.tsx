import { ThemedIcon } from "./themed-icon";

export function PolicyUpdateBanner({
  count,
  onReview,
}: {
  count: number;
  onReview: () => void;
}) {
  if (count === 0) return null;
  return (
    <div
      className="mx-2 mb-2 flex items-center rounded-2xl bg-destructive/[0.08] px-4"
      role="status"
    >
      <span className="mr-3 flex size-11 shrink-0 items-center justify-center">
        <ThemedIcon
          className="size-8 text-destructive"
          src="/wallet-workspace/facelift/settings/warning-large.svg"
        />
      </span>
      <div className="min-w-0 flex-1 py-[11px]">
        <p className="font-medium text-[16px] leading-5 tracking-[-0.176px]">
          Policy updates required
        </p>
        <p className="mt-0.5 text-[13px] leading-4 text-muted-foreground">
          {count} onchain {count === 1 ? "policy has" : "policies have"} been
          updated. Sign the transactions to apply the latest versions
        </p>
      </div>
      <button
        className="t-hover ml-3 shrink-0 rounded-full bg-primary px-4 py-2.5 font-medium text-[13px] leading-4 text-white hover:bg-primary/90"
        onClick={onReview}
        type="button"
      >
        Update
      </button>
    </div>
  );
}
