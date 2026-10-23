// Categorical series slots for the metrics dashboard. The slot order is the
// colour-vision-deficiency safety mechanism, so slots are assigned by series
// identity and are never reordered or cycled per chart.
export const METRIC_SERIES_COLORS = [
  "#2a78d6",
  "#eb6834",
  "#1baf7a",
  "#eda100",
  "#e87ba4",
  "#008300",
] as const;

// Same six hues, re-stepped for the dark card surface — not an automatic flip.
export const METRIC_SERIES_COLORS_DARK = [
  "#3987e5",
  "#d95926",
  "#199e70",
  "#c98500",
  "#d55181",
  "#008300",
] as const;

export const SERIES_SLOT_COUNT = METRIC_SERIES_COLORS.length;

const metricVars = (colors: readonly string[]) =>
  colors.map((color, index) => `--metric-${index + 1}:${color};`).join("");

export const METRIC_SERIES_CSS = `:root{${metricVars(METRIC_SERIES_COLORS)}}.dark{${metricVars(METRIC_SERIES_COLORS_DARK)}}`;
