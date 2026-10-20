import type { NextFont } from "./font-google";

import "./fonts.css";

// The face is declared in fonts.css from the geist package's variable woff2;
// this class only defines the variable globals.css reads.
export const GeistSans: NextFont = { className: "", variable: "font-geist-sans", style: { fontFamily: "'Geist Sans', sans-serif" } };
