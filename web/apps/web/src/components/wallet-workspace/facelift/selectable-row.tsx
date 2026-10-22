import type { ReactNode } from "react";

export function SelectableRow({
  children,
  isSelected,
  onSelect,
}: {
  children: ReactNode;
  isSelected: boolean;
  onSelect?: () => void;
}) {
  if (!onSelect) {
    return (
      <div className="flex w-full items-center rounded-2xl px-4">{children}</div>
    );
  }
  return (
    <button
      className={`flex w-full items-center rounded-2xl px-4 text-left outline-none transition-colors duration-150 focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:ring-inset ${
        isSelected ? "bg-accent" : "hover:bg-accent"
      }`}
      onClick={onSelect}
      type="button"
    >
      {children}
    </button>
  );
}
