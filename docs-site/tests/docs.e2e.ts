import { readFileSync, readdirSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { test } from "@e2e-dev/web";
import { expect } from "e2e";
const root = "content/docs";
function pages(directory = root): { route: string; title: string }[] {
 return readdirSync(directory, { withFileTypes: true }).flatMap(entry => {
  const path = join(directory, entry.name);
  if (entry.isDirectory()) return pages(path);
  if (!entry.name.endsWith(".mdx")) return [];
  const title = readFileSync(path, "utf8").match(/^title:\s*(.+)$/m)?.[1]?.replace(/^"|"$/g, "");
  if (!title) throw new Error(`Missing title: ${path}`);
  const route = "/docs/" + relative(root, path).split(sep).join("/").replace(/\.mdx$/, "").replace(/(^|\/)index$/, "");
  return [{ route, title }];
 });
}
for (const page of pages()) {
 test(`documentation ${page.route}`, async ({ app, screen }) => {
  await app.open(page.route);
  await expect(screen.getByRole("heading", { name: page.title, exact: true })).toBeVisible();
 });
}
test("home opens docs; mobile layout and search index remain available", async ({ app, browser, screen }) => {
 await browser.setViewport({ width: 390, height: 844 });
 await app.open("/");
 await expect(screen.getByRole("heading", { name: pages().find(p => p.route === "/docs/")!.title, exact: true })).toBeVisible();
 const result = await browser.evaluate(async () => {
  const response = await fetch("/api/search");
  return { status: response.status, data: await response.json() };
 });
 expect(result.status).toBe(200);
 const indexedPages = Object.values(result.data.docs.docs) as {type:string;url:string;content:string}[];
 expect(indexedPages.some(page => page.type === "page" && page.url === "/docs/auth" && page.content.length > 0)).toBe(true);
 expect(await browser.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
});
