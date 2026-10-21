import "./fonts.css";

export type NextFont = { className: string; variable: string; style: { fontFamily: string } };

type GoogleFontOptions = {
  weight?: string | string[];
  style?: string | string[];
  subsets?: string[];
  variable?: string;
  display?: string;
};

export function IBM_Plex_Sans(options: GoogleFontOptions = {}): NextFont {
  const variable = (options.variable ?? "--font-ibm-plex-sans").replace(/^--/, "");
  return {
    className: "",
    variable,
    style: { fontFamily: `var(--${variable}), sans-serif` },
  };
}
