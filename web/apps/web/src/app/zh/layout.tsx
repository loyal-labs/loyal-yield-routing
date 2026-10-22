export default function ZhLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return <div lang="zh-Hans">{children}</div>;
}
