/**
 * Pure lane-selection helpers for the Phase-2 K-Lend obligation prerequisites
 * script: parsing and validating the RWA_OBLIGATION_LANES operator filter, and
 * refusing journals written under a different filter.  Deliberately free of
 * Solana, RPC, and filesystem imports so the bun tests exercise them directly.
 */
type Json = Record<string, unknown>;

export const LANE_FILTER_ENV = "RWA_OBLIGATION_LANES";
export type LaneFilter = Readonly<{ keys: readonly string[] }>;

function render(value: unknown): string { return JSON.stringify(value); }

/**
 * Parses the comma-separated lane filter into an ordered list of exact
 * resolution lane keys.  Refuses an unset, empty, or blank value, unknown keys,
 * empty entries, and duplicates, so callers can fail before any RPC call.
 */
export function parseLaneFilter(rawValue: string | undefined, laneKeys: readonly string[]): LaneFilter {
  const raw = rawValue?.trim() ?? "";
  if (raw.length === 0) throw new Error(`${LANE_FILTER_ENV} is required: set it to a comma-separated list of resolution lane keys such as ${render(laneKeys)}`);
  const keys = raw.split(",").map((entry) => entry.trim());
  const empty = keys.find((entry) => entry.length === 0);
  if (empty !== undefined) throw new Error(`${LANE_FILTER_ENV} contains an empty lane key: ${render(raw)}; use exact resolution lane keys such as ${render(laneKeys)}`);
  const unknown = keys.find((key) => !laneKeys.includes(key));
  if (unknown !== undefined) throw new Error(`${LANE_FILTER_ENV} names unknown lane ${render(unknown)}; exact resolution lanes are ${render(laneKeys)}`);
  const duplicate = keys.find((key, index) => keys.indexOf(key) !== index);
  if (duplicate !== undefined) throw new Error(`${LANE_FILTER_ENV} repeats lane ${render(duplicate)}; list each lane at most once`);
  return { keys };
}

/** Returns the resolved lanes the filter names, in the filter's own order. */
export function selectLanes<T extends { key: string }>(lanes: readonly T[], filter: LaneFilter): readonly T[] {
  return filter.keys.map((key) => {
    const lane = lanes.find((candidate) => candidate.key === key);
    if (!lane) throw new Error(`${LANE_FILTER_ENV} lane ${render(key)} is absent from the resolution artifact`);
    return lane;
  });
}

/**
 * Refuses to resume a journal that was written under a different lane filter,
 * including journals written before the filter existed.
 */
export function assertJournalLaneFilter(journal: Json, filter: LaneFilter, label: string): void {
  const recorded = journal.laneFilter;
  const keys = Array.isArray(recorded) && recorded.every((entry): entry is string => typeof entry === "string") ? recorded : null;
  if (!keys) throw new Error(`${label} has no laneFilter array; refusing to resume a journal written before ${LANE_FILTER_ENV} existed`);
  if (render(keys) !== render(filter.keys)) throw new Error(`${label} laneFilter ${render(keys)} does not match the current ${LANE_FILTER_ENV} ${render(filter.keys)}; refusing to resume across a lane-filter change`);
}
