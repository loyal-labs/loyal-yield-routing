export const siteJsonLd = {
  "@context": "https://schema.org",
  "@graph": [
    {
      "@type": "Organization",
      "@id": "https://askloyal.com/#organization",
      name: "Loyal",
      legalName: "Loyal DAO LLC",
      url: "https://askloyal.com",
      logo: "https://askloyal.com/android-chrome-512x512.png",
      description:
        "Self-custody Solana smart-account wallet that earns the best available stablecoin yield automatically. On-chain guardrails for AI agents: spending caps, token whitelists, and approved-protocol allowlists. Plus private payments.",
      foundingDate: "2025",
      foundingLocation: {
        "@type": "Place",
        name: "Marshall Islands",
      },
      contactPoint: {
        "@type": "ContactPoint",
        contactType: "customer support",
        email: "main@askloyal.com",
        url: "https://discord.askloyal.com",
      },
      sameAs: [
        "https://www.wikidata.org/wiki/Q139927376",
        "https://x.com/loyal_hq",
        "https://github.com/loyal-labs",
        "https://discord.askloyal.com",
        "https://t.me/loyal_tgchat",
        "https://medium.com/@askloyal",
        "https://chromewebstore.google.com/detail/cdienfadefhlaknmedckgifkjdbioack",
        "https://app.askloyal.com",
      ],
    },
    {
      "@type": "WebSite",
      "@id": "https://askloyal.com/#website",
      url: "https://askloyal.com",
      name: "Loyal",
      publisher: { "@id": "https://askloyal.com/#organization" },
      inLanguage: "en-US",
    },
    {
      "@type": "SoftwareApplication",
      "@id": "https://askloyal.com/#app",
      name: "Loyal",
      url: "https://askloyal.com",
      image: "https://askloyal.com/og-home-2026-08.png",
      applicationCategory: "FinanceApplication",
      operatingSystem: "Android",
      description:
        "Solana wallet that earns stablecoin yield automatically, with smart-account guardrails for AI agents and private transfers. Available on web, Chrome extension, Telegram mini app, Android (Google Play), and Solana Mobile (Seeker).",
      publisher: { "@id": "https://askloyal.com/#organization" },
      offers: {
        "@type": "Offer",
        price: "0",
        priceCurrency: "USD",
      },
    },
  ],
};
