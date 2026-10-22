import { IBM_Plex_Sans } from "next/font/google";

export const ibmPlexSans = IBM_Plex_Sans({
  weight: ["400"],
  style: ["normal", "italic"],
  subsets: ["latin"],
  variable: "--font-ibm-plex-sans",
  display: "swap",
});
