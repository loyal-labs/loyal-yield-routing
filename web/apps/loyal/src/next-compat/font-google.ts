import "./fonts.css";

export type NextFont = { className: string; variable: string; style: { fontFamily: string } };

type PlexFace = "normal" | "italic";

type GoogleFontOptions = {
  weight?: "400" | ["400"];
  style?: PlexFace | PlexFace[];
  subsets?: string[];
  variable?: string;
  display?: string;
};

export function IBM_Plex_Sans(options: GoogleFontOptions = {}): NextFont {
  const variable = (options.variable ?? "--font-ibm-plex-sans").replace(/^--/, "");
  return {
    className: "",
    variable,
    style: { fontFamily: "'IBM Plex Sans', sans-serif" },
  };
}

export function Geist(_options: GoogleFontOptions = {}): NextFont {
  return { className: "", variable: "font-geist-sans", style: { fontFamily: "'Geist Sans', sans-serif" } };
}

export function Geist_Mono(_options: GoogleFontOptions = {}): NextFont {
  return { className: "", variable: "font-geist-mono", style: { fontFamily: "'Geist Mono', monospace" } };
}
