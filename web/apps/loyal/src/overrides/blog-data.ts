// Vite stand-in for apps/web src/features/blog/data.ts, which reads
// public/blog/<slug>/post.md with node:fs at request time. Same exports, same
// types, same sorting and pagination — but the markdown is inlined at build
// time with import.meta.glob, because the browser has no node:fs.
//
// The glob and the parsing live in blog-posts.ts, reached only through the
// dynamic import below: the entry chunk is downloaded on every page, so the
// post bodies must stay in one lazy chunk that the blog pages pull.
import type { BlogPageResult, BlogPost, BlogPostFull } from "@/features/blog/types";

export const BLOG_POSTS_PER_PAGE = 9;

let allPostsPromise: Promise<BlogPostFull[]> | undefined;

function loadAllPosts(): Promise<BlogPostFull[]> {
  return (allPostsPromise ??= import("./blog-posts").then((m) => m.allPosts));
}

function toMeta(post: BlogPostFull): BlogPost {
  return {
    slug: post.slug,
    title: post.title,
    date: post.date,
    updated: post.updated,
    hero: post.hero,
    description: post.description,
    author: post.author,
    readingTime: post.readingTime,
  };
}

function clamp(value: number, min: number, max: number): number {
  return Math.min(Math.max(value, min), max);
}

export async function getBlogPosts({
  page = 1,
  perPage = BLOG_POSTS_PER_PAGE,
}: { page?: number; perPage?: number } = {}): Promise<BlogPageResult> {
  const allPosts = await loadAllPosts();
  const total = allPosts.length;
  const totalPages = Math.max(1, Math.ceil(total / perPage));
  const safePage = clamp(Math.trunc(page) || 1, 1, totalPages);
  const start = (safePage - 1) * perPage;
  const posts = allPosts.slice(start, start + perPage).map(toMeta);

  return { posts, page: safePage, perPage, total, totalPages };
}

export async function getBlogPost(slug: string): Promise<BlogPostFull | null> {
  const allPosts = await loadAllPosts();
  return allPosts.find((post) => post.slug === slug) ?? null;
}

export async function getMoreArticles(
  slug: string,
  count = 3
): Promise<BlogPost[]> {
  const allPosts = await loadAllPosts();
  return allPosts
    .filter((post) => post.slug !== slug)
    .slice(0, count)
    .map(toMeta);
}

export async function getAllBlogSlugs(): Promise<string[]> {
  const allPosts = await loadAllPosts();
  return allPosts.map((post) => post.slug);
}

export async function getAllBlogPosts(): Promise<BlogPost[]> {
  const allPosts = await loadAllPosts();
  return allPosts.map(toMeta);
}
