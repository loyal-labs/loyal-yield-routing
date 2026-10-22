import {
  Banknote,
  Building2,
  CircleCheck,
  FileCode,
  KeyRound,
  Network,
  ShieldCheck,
  Sprout,
  TrendingDown,
  Vault,
} from "lucide-react";

import { LandingFaq } from "@/components/landing-faq";
import { LandingFooter } from "@/components/landing-footer";
import { LandingHeader } from "@/components/landing-header";
import { LandingScrollAnimations } from "@/components/landing-scroll-animations";
import { CardsGrid } from "@/features/marketing/blocks/cards-grid";
import { Hero } from "@/features/marketing/blocks/hero";
import { Section } from "@/features/marketing/blocks/section";
import { TextImageHero } from "@/features/marketing/blocks/text-image";
import type { CommonDict } from "@/features/marketing/i18n/en/common";
import type { EarnDict } from "@/features/marketing/i18n/en/earn";
import type { Locale } from "@/features/marketing/i18n/locale";
import { buildBreadcrumbJsonLd } from "@/features/marketing/i18n/metadata";

export const EARN_OG_IMAGE = {
  url: "/og-image.png",
  width: 1200,
  height: 630,
};

export function EarnPage({
  locale,
  dict,
  common,
}: {
  locale: Locale;
  dict: EarnDict;
  common: CommonDict;
}) {
  const breadcrumbJsonLd = buildBreadcrumbJsonLd({
    locale,
    path: "/earn",
    copy: dict.breadcrumb,
  });

  return (
    <main className="min-h-screen overflow-x-clip bg-white text-black">
      {/* JSON-LD as script children (XSS-safe; React escapes <>&) — schema has no such chars */}
      <script type="application/ld+json">
        {JSON.stringify(breadcrumbJsonLd)}
      </script>

      <LandingScrollAnimations />
      <LandingHeader
        copy={common.header}
        locale={locale}
      />

      <Hero
        tone="dark"
        title={dict.hero.title}
        body={dict.hero.body}
        cta={{ label: dict.hero.cta, href: "https://app.askloyal.com" }}
        image={{
          src: "/landing/figma/feature-yield-card.png",
          alt: dict.hero.imageAlt,
        }}
      />

      <Section
        title={dict.howItEarns.title}
        description={dict.howItEarns.description}
        cards={[
          {
            id: "howItEarns-optimized",
            size: "lg",
            body: dict.howItEarns.cards.optimized,
          },
          {
            id: "howItEarns-bounded",
            size: "lg",
            body: dict.howItEarns.cards.bounded,
          },
        ]}
      />

      <Section
        title={dict.dollars.title}
        cards={[
          {
            id: "dollars-interchangeable",
            size: "lg",
            body: dict.dollars.cards.interchangeable,
          },
          {
            id: "dollars-rotation",
            size: "lg",
            body: dict.dollars.cards.rotation,
          },
        ]}
      />

      <Section
        title={dict.rateSpikes.title}
        description={dict.rateSpikes.description}
        cards={[
          {
            id: "rateSpikes-supplyUtilization",
            size: "lg",
            body: dict.rateSpikes.cards.supplyUtilization,
          },
          {
            id: "rateSpikes-opening",
            size: "lg",
            body: dict.rateSpikes.cards.opening,
          },
        ]}
      />

      <Section
        title={dict.routing.title}
        cards={[
          {
            id: "routing-motion",
            size: "lg",
            body: dict.routing.cards.motion,
          },
          {
            id: "routing-automation",
            size: "lg",
            body: dict.routing.cards.automation,
          },
        ]}
      />

      <CardsGrid
        title={dict.ruledOut.title}
        description={dict.ruledOut.description}
        variant="muted"
        columns={3}
        cards={[
          {
            icon: <FileCode className="size-16 text-[#f9363c]" />,
            title: dict.ruledOut.cards.contract.title,
            body: dict.ruledOut.cards.contract.body,
          },
          {
            icon: <KeyRound className="size-16 text-[#f9363c]" />,
            title: dict.ruledOut.cards.backendKey.title,
            body: dict.ruledOut.cards.backendKey.body,
          },
          {
            icon: <Vault className="size-16 text-[#f9363c]" />,
            title: dict.ruledOut.cards.vault.title,
            body: dict.ruledOut.cards.vault.body,
          },
        ]}
      />

      <Section
        title={dict.policy.title}
        description={dict.policy.description}
        cards={[
          {
            id: "policy-intents",
            size: "lg",
            body: dict.policy.cards.intents,
          },
          {
            id: "policy-autoApproved",
            size: "lg",
            body: dict.policy.cards.autoApproved,
          },
        ]}
      />

      <CardsGrid
        title={dict.risks.title}
        description={dict.risks.description}
        variant="muted"
        columns={2}
        cards={[
          {
            icon: <TrendingDown className="size-16 text-[#f9363c]" />,
            title: dict.risks.cards.reserve.title,
            body: dict.risks.cards.reserve.body,
          },
          {
            icon: <ShieldCheck className="size-16 text-[#f9363c]" />,
            title: dict.risks.cards.custody.title,
            body: dict.risks.cards.custody.body,
          },
          {
            icon: <CircleCheck className="size-16 text-[#f9363c]" />,
            title: dict.risks.cards.noLiquidations.title,
            body: dict.risks.cards.noLiquidations.body,
          },
          {
            icon: <KeyRound className="size-16 text-[#f9363c]" />,
            title: dict.risks.cards.openSource.title,
            body: dict.risks.cards.openSource.body,
          },
        ]}
      />

      <CardsGrid
        title={dict.audiences.title}
        variant="muted"
        columns={2}
        cards={[
          {
            icon: <Building2 className="size-16 text-[#f9363c]" />,
            title: dict.audiences.cards.treasuries.title,
            body: dict.audiences.cards.treasuries.body,
          },
          {
            icon: <Network className="size-16 text-[#f9363c]" />,
            title: dict.audiences.cards.daos.title,
            body: dict.audiences.cards.daos.body,
          },
          {
            icon: <Banknote className="size-16 text-[#f9363c]" />,
            title: dict.audiences.cards.runway.title,
            body: dict.audiences.cards.runway.body,
          },
          {
            icon: <Sprout className="size-16 text-[#f9363c]" />,
            title: dict.audiences.cards.farmers.title,
            body: dict.audiences.cards.farmers.body,
          },
        ]}
      />

      <TextImageHero
        title={dict.howItsBuilt.title}
        body={dict.howItsBuilt.body}
        cta={{
          label: dict.howItsBuilt.cta,
          href: "https://docs.askloyal.com",
        }}
        image={{
          src: "/marketing/agents/dev-sdk-card.53826a2b.png",
          alt: dict.howItsBuilt.imageAlt,
        }}
      />

      <TextImageHero
        layout="text-right"
        title={dict.startEarning.title}
        body={dict.startEarning.body}
        cta={{ label: dict.startEarning.cta, href: "https://app.askloyal.com" }}
        image={{
          src: "/landing/figma/get-started-extension-wallet.png",
          alt: dict.startEarning.imageAlt,
        }}
      />

      <LandingFaq
        heading={common.faq.heading}
        items={dict.faqs}
        locale={locale}
      />
      <LandingFooter
        copy={common.footer}
        languageSwitchLabel={common.languageSwitch.ariaLabel}
        locale={locale}
        path="/earn"
      />
    </main>
  );
}
