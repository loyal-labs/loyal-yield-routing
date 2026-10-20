import "./fonts.css";

export type NextFont = { className: string; variable: string; style: { fontFamily: string } };

type GoogleFontOptions = {
  weight?: string | string[];
  style?: string | string[];
  subsets?: string[];
  variable?: string;
  display?: string;
};

// IBM Plex Sans is self-hosted in ./fonts (400 normal + italic, OFL), declared
// in fonts.css. Only the blog requests it, with weight 400 and both styles —
// exactly what fonts.css declares; other option values would need more faces
// and are not modelled.
export function IBM_Plex_Sans(options: GoogleFontOptions = {}): NextFont {
  return {
    className: "",
    variable: (options.variable ?? "--font-ibm-plex-sans").replace(/^--/, ""),
    style: { fontFamily: "'IBM Plex Sans', sans-serif" },
  };
}
