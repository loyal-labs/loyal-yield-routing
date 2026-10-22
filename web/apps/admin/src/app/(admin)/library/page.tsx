import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";

import { LibraryList } from "./library-list";

type LibraryPageProps = {
  sections: Array<{
    id: string;
    title: string;
    displayOrder: number;
    isActive: boolean;
    createdAt: string;
    updatedAt: string;
  }>;
  articles: Array<{
    id: string;
    sectionId: string;
    title: string;
    coverImageUrl: string;
    contentMarkdown: string;
    readTime: string;
    excerpt: string | null;
    isFeatured: boolean;
    isActive: boolean;
    displayOrder: number;
    publishedAt: string | null;
    createdAt: string;
    updatedAt: string;
  }>;
};

export default function LibraryPage({ sections, articles }: LibraryPageProps) {
  return (
    <PageContainer>
      <SectionHeader title="Library" breadcrumbs={[{ label: "Library" }]} />
      <LibraryList sections={sections} articles={articles} />
    </PageContainer>
  );
}
