import type { BlogPageResult, BlogPost, BlogPostFull } from "./types";

export const BLOG_POSTS_PER_PAGE = 9;

export function toBlogPostMeta(post: BlogPostFull): BlogPost {
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

export function toBlogPageResult(
  all: BlogPostFull[],
  {
    page = 1,
    perPage = BLOG_POSTS_PER_PAGE,
  }: { page?: number; perPage?: number } = {}
): BlogPageResult {
  const total = all.length;
  const totalPages = Math.max(1, Math.ceil(total / perPage));
  const safePage = clamp(Math.trunc(page) || 1, 1, totalPages);
  const start = (safePage - 1) * perPage;
  const posts = all.slice(start, start + perPage).map(toBlogPostMeta);

  return { posts, page: safePage, perPage, total, totalPages };
}
