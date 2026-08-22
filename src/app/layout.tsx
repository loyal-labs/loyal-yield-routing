import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Loyal Admin · Vault Monitoring",
  description: "Operational monitoring for Loyal-managed yield vaults",
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
