// The closure between a page, its markdown twin and the llms.txt indexes.
//
// Each of those three is generated, and each of them is a valid document when
// it is wrong: a page whose twin was never emitted still renders, a twin left
// behind by a deleted page still parses, an index still lists a URL that now
// 404s. Nothing else in the pipeline reads them, so this is the class of drift
// that ships silently and is only noticed by whoever fetched the twin instead
// of the page, which is a machine, which does not file bugs.
//
// Usage:
//   node scripts/check-twins.mjs [dist-directory]
//   node scripts/check-twins.mjs --only twins   # page and twin closure
//   node scripts/check-twins.mjs --only llms    # the two llms.txt indexes
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { join, resolve } from "node:path";
import process from "node:process";

const args = process.argv.slice(2);
const positional = [];
let only = "all";
for (let i = 0; i < args.length; i += 1) {
	if (args[i] === "--only") {
		i += 1;
		only = args[i];
	} else {
		positional.push(args[i]);
	}
}
if (!["all", "twins", "llms"].includes(only)) {
	console.error(`[twins] unknown --only ${only}. Use "twins" or "llms".`);
	process.exit(1);
}
// An explicit directory is for reproducing a defect against a build kept
// aside; with none, the check reads the build that is there.
const DIST = resolve(positional[0] ?? "dist");

// The site root as the build itself states it, rather than as a constant this
// file would have to be kept in step with. The sitemap index is written by the
// sitemap integration from the same `site` and `base` every URL on the site is
// built from.
const sitemapIndex = join(DIST, "sitemap-index.xml");
if (!existsSync(sitemapIndex)) {
	console.error(
		`[twins] no sitemap-index.xml under ${DIST}. Run pnpm build first.`,
	);
	process.exit(1);
}
const SITE_ROOT = /<loc>([^<]+)\/sitemap-\d+\.xml<\/loc>/.exec(
	readFileSync(sitemapIndex, "utf8"),
)?.[1];
if (!SITE_ROOT) {
	console.error(
		"[twins] could not read the site root out of sitemap-index.xml",
	);
	process.exit(1);
}

const problems = [];
const fail = (message) => problems.push(message);

/** Every file under dist matching a name test, as dist-relative paths. */
function* files(dir, matches, prefix = "") {
	for (const entry of readdirSync(dir, { withFileTypes: true })) {
		const relative = prefix ? `${prefix}/${entry.name}` : entry.name;
		if (entry.isDirectory())
			yield* files(join(dir, entry.name), matches, relative);
		else if (entry.isFile() && matches(entry.name)) yield relative;
	}
}

// The not-found page is not a route: it is one file GitHub Pages serves for
// every URL that does not exist. It has no twin, and no index lists it.
const NOT_A_PAGE = new Set(["404.html"]);

const pages = [...files(DIST, (name) => name === "index.html")]
	.filter((path) => !NOT_A_PAGE.has(path))
	.map((path) => path.replace(/index\.html$/, ""));
const twins = new Set([...files(DIST, (name) => name === "index.md")]);

if (only !== "llms") {
	if (pages.length === 0) {
		fail(`no built pages under ${DIST}. Run pnpm build first.`);
	}

	for (const page of pages) {
		const twin = `${page}index.md`;
		if (!twins.has(twin)) {
			fail(`/${page} has no markdown twin at /${twin}`);
			continue;
		}
		// An empty twin is worse than a missing one: it answers the request, so
		// whatever fetched it reads the page as having no content.
		const text = readFileSync(join(DIST, twin), "utf8");
		if (!/^#\s+\S/m.test(text) || text.trim().split("\n").length < 4) {
			fail(
				`/${twin} is empty or has no heading (${statSync(join(DIST, twin)).size} bytes)`,
			);
		}

		// The announcement, which is how anything finds the twin without guessing
		// the convention.
		const html = readFileSync(join(DIST, `${page}index.html`), "utf8");
		const link =
			/<link[^>]*rel="alternate"[^>]*type="text\/markdown"[^>]*>/.exec(html);
		if (!link) {
			fail(
				`/${page} does not announce its twin with <link rel="alternate" type="text/markdown">`,
			);
			continue;
		}
		const href = /href="([^"]+)"/.exec(link[0])?.[1];
		const target = href?.startsWith(SITE_ROOT)
			? href.slice(SITE_ROOT.length).replace(/^\//, "")
			: href?.replace(/^\/[^/]+\//, "");
		if (!href || !target || !existsSync(join(DIST, target))) {
			fail(
				`/${page} announces a twin at ${href}, which the build does not contain`,
			);
		}
	}

	for (const twin of twins) {
		if (!pages.includes(twin.replace(/index\.md$/, ""))) {
			fail(`/${twin} has no page: the twin outlived whatever it doubled`);
		}
	}
}

if (only !== "twins") {
	for (const index of [
		"llms.txt",
		"es/llms.txt",
		"llms-full.txt",
		"es/llms-full.txt",
	]) {
		const path = join(DIST, index);
		if (!existsSync(path)) {
			fail(`/${index} was not generated`);
			continue;
		}
		const text = readFileSync(path, "utf8");
		if (text.trim().length === 0) {
			fail(`/${index} is empty`);
			continue;
		}
		if (!index.endsWith("llms-full.txt")) {
			const listed = [...text.matchAll(/\]\((https?:\/\/[^)]+)\)/g)].map(
				(m) => m[1],
			);
			if (listed.length === 0) fail(`/${index} lists no pages`);
			for (const url of listed) {
				if (!url.startsWith(SITE_ROOT)) continue; // an outbound link, not a page
				const relative = url.slice(SITE_ROOT.length).replace(/^\//, "");
				const target =
					relative.endsWith("/") || relative === ""
						? `${relative}index.html`
						: relative;
				if (!existsSync(join(DIST, target))) {
					fail(`/${index} lists ${url}, which the build does not contain`);
				}
			}

			// The other direction: a page the index does not list is a page no model
			// reading this file will ever see.
			const locale = index.startsWith("es/") ? "es" : "en";
			const ours = pages.filter(
				(page) =>
					(page.startsWith("es/") || page === "es/") === (locale === "es"),
			);
			for (const page of ours) {
				if (!listed.includes(`${SITE_ROOT}/${page}`)) {
					fail(`/${index} does not list /${page}`);
				}
			}
		}
	}
}

if (problems.length > 0) {
	console.error(`[twins] ${problems.length} problem(s):`);
	for (const problem of problems) console.error(`  ${problem}`);
	process.exit(1);
}

console.log(
	only === "llms"
		? "[twins] both llms.txt indexes list pages that exist, and every page is listed."
		: only === "twins"
			? `[twins] ${pages.length} pages, ${twins.size} twins, each announced and each with a page.`
			: `[twins] ${pages.length} pages, ${twins.size} twins, and both llms.txt indexes closed.`,
);
