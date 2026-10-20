// Vite stand-in for apps/web src/features/blog/data.ts, which reads
// public/blog/<slug>/post.md with node:fs at request time. Same exports, same
// types. Seed body: no posts. G4 reads the posts at build with import.meta.glob.
import type { BlogPageResult, BlogPost, BlogPostFull } from "@/features/blog/types";

export const BLOG_POSTS_PER_PAGE = 9;

export async function getBlogPosts({ page }: { page: number }): Promise<BlogPageResult> {
  return { posts: [], page, totalPages: 1 } as unknown as BlogPageResult;
}

export async function getBlogPost(_slug: string): Promise<BlogPostFull | null> {
  return null;
}

export async function getMoreArticles(_slug: string, _count?: number): Promise<BlogPost[]> {
  return [];
}

export async function getAllBlogSlugs(): Promise<string[]> {
  return [];
}

export async function getAllBlogPosts(): Promise<BlogPost[]> {
  return [];
}
