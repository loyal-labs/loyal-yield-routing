export type NextFont = { className: string; variable: string; style: { fontFamily: string } };

type GoogleFontOptions = {
  weight?: string | string[];
  style?: string | string[];
  subsets?: string[];
  variable?: string;
  display?: string;
};

// Seed body: names the family and exposes the variable. G1 loads the face.
export function IBM_Plex_Sans(options: GoogleFontOptions): NextFont {
  const variable = options.variable?.replace(/^--/, "") ?? "font-ibm-plex-sans";
  return { className: "", variable, style: { fontFamily: "'IBM Plex Sans', sans-serif" } };
}
