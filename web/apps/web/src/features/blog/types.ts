export type BlogAuthor = {
  name: string;
  avatar?: string;
};

export type BlogPost = {
  slug: string;
  title: string;
  date: string;
  updated?: string;
  hero: string;
  description?: string;
  author?: BlogAuthor;
  readingTime: number;
};

export type BlogPostFull = BlogPost & {
  content: string;
};

export type BlogPageResult = {
  posts: BlogPost[];
  page: number;
  perPage: number;
  total: number;
  totalPages: number;
};
