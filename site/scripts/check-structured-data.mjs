// The JSON-LD graph the site publishes, read back out of the build.
//
// It is the one part of a page written for machines alone, and until this
// check existed nothing in the repository read it. html-validate does not look
// inside a <script>, htmlhint does not either, pa11y has no reason to, and a
// link checker sees no links there. A graph could stop being JSON, lose the
// node that carries the author, or point every reference at an @id that is not
// in the document, and every other check would stay green.
//
// What it asserts is derived from each page's own URL and from the build
// around it, not from what src/overrides/Head.astro believes: a check that
// asked the emitter what it emits would agree with the emitter's defects.
//
// Usage: node scripts/check-structured-data.mjs [dist-directory]
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { join, resolve } from "node:path";
import process from "node:process";

const DIST = resolve(process.argv[2] ?? "dist");
const PERSON_ID = "https://jmrp.io/#person";

const sitemapIndex = join(DIST, "sitemap-index.xml");
if (!existsSync(sitemapIndex)) {
	console.error(
		`[schema] no sitemap-index.xml under ${DIST}. Run pnpm build first.`,
	);
	process.exit(1);
}
// The site root as the build states it, so a domain or base rename needs no
// edit here.
const SITE_ROOT = /<loc>([^<]+)\/sitemap-\d+\.xml<\/loc>/.exec(
	readFileSync(sitemapIndex, "utf8"),
)?.[1];
if (!SITE_ROOT) {
	console.error(
		"[schema] could not read the site root out of sitemap-index.xml",
	);
	process.exit(1);
}
const BASE_PATH = new URL(SITE_ROOT).pathname.replace(/\/$/, "");

const problems = [];

function* htmlFiles(dir, prefix = "") {
	for (const entry of readdirSync(dir, { withFileTypes: true })) {
		const relative = prefix ? `${prefix}/${entry.name}` : entry.name;
		if (entry.isDirectory()) yield* htmlFiles(join(dir, entry.name), relative);
		else if (entry.isFile() && entry.name.endsWith(".html")) yield relative;
	}
}

/** The dist path a site URL addresses, or undefined if it is not on this site. */
function distPathOf(url) {
	if (!url.startsWith(SITE_ROOT)) return undefined;
	const relative = url.slice(SITE_ROOT.length).replace(/^\//, "");
	return relative === "" || relative.endsWith("/")
		? `${relative}index.html`
		: relative;
}

let pages = 0;
for (const file of htmlFiles(DIST)) {
	const html = readFileSync(join(DIST, file), "utf8");
	const page = `/${file.replace(/index\.html$/, "")}`;
	const report = (message) => problems.push(`${page}: ${message}`);
	pages += 1;

	const scripts = [
		...html.matchAll(
			/<script[^>]+type="application\/ld\+json"[^>]*>([\s\S]*?)<\/script>/g,
		),
	];
	if (scripts.length !== 1) {
		report(`expected exactly 1 JSON-LD script, found ${scripts.length}`);
		continue;
	}
	const raw = scripts[0][1];
	// The serializer escapes these, so a page carrying one raw means a title or
	// description reached the document unescaped and could close the element.
	if (/[<>]/.test(raw)) {
		report(
			"the JSON-LD carries a raw < or >, which can break out of the script",
		);
	}

	let graph;
	try {
		graph = JSON.parse(raw);
	} catch (error) {
		report(`the JSON-LD does not parse: ${error.message}`);
		continue;
	}
	if (graph["@context"] !== "https://schema.org") {
		report(
			`@context is ${JSON.stringify(graph["@context"])}, expected https://schema.org`,
		);
	}
	const nodes = graph["@graph"];
	if (!Array.isArray(nodes) || nodes.length === 0) {
		report("no @graph array");
		continue;
	}

	const types = new Set(nodes.map((node) => node["@type"]));
	const isHome = page === "/" || page === "/es/";
	const isNotFound = file === "404.html";
	const expected = [
		"Person",
		"WebSite",
		"SoftwareApplication",
		"SoftwareSourceCode",
		...(isNotFound ? [] : [isHome ? "WebPage" : "TechArticle"]),
		...(isNotFound || isHome ? [] : ["BreadcrumbList"]),
	];
	for (const type of expected) {
		if (!types.has(type)) report(`the graph has no ${type} node`);
	}

	const person = nodes.find((node) => node["@type"] === "Person");
	if (person && person["@id"] !== PERSON_ID) {
		report(
			`the Person node is ${person["@id"]}, expected the canonical ${PERSON_ID}`,
		);
	}
	if (person && !person.name) {
		report(
			"the Person node carries no name, so the identity fetch returned nothing usable",
		);
	}

	// Every reference this site writes either names a node of this graph or the
	// canonical person, which is published as its own document elsewhere. A
	// reference to anything else is a pointer no consumer can resolve.
	//
	// The Person node is exempt because it is not this site's to answer for: it
	// arrives from jmrp.io already pointing at that account's other projects and
	// profiles, which are documents in their own right and are not part of this
	// graph by design.
	const ids = new Set(nodes.map((node) => node["@id"]).filter(Boolean));
	const references = [
		...JSON.stringify(
			nodes.filter((node) => node["@type"] !== "Person"),
		).matchAll(/\{"@id":"([^"]+)"\}/g),
	];
	for (const [, id] of references) {
		if (!ids.has(id) && id !== PERSON_ID) {
			report(`references ${id}, which is not a node of the graph`);
		}
	}

	const document = nodes.find(
		(node) => node["@type"] === "TechArticle" || node["@type"] === "WebPage",
	);
	if (document) {
		const canonical = `${new URL(SITE_ROOT).origin}${BASE_PATH}${page}`;
		if (document.url !== canonical) {
			report(
				`the document node says url ${document.url}, expected ${canonical}`,
			);
		}
		// The twin it announces has to exist, or the cheap representation is a 404.
		const twin = document.encoding?.contentUrl;
		const twinPath = twin && distPathOf(twin);
		if (!twin || !twinPath || !existsSync(join(DIST, twinPath))) {
			report(
				`announces its markdown at ${twin}, which the build does not contain`,
			);
		}
	}

	const breadcrumb = nodes.find((node) => node["@type"] === "BreadcrumbList");
	if (breadcrumb) {
		const items = breadcrumb.itemListElement ?? [];
		items.forEach((item, index) => {
			if (item.position !== index + 1) {
				report(`breadcrumb item ${index + 1} is at position ${item.position}`);
			}
			if (!item.name) report(`breadcrumb item ${index + 1} has no name`);
			// A crumb may be a name without a link, which is the documented form for
			// a section with no index page. A crumb that does link must not 404.
			const target = item.item && distPathOf(item.item);
			if (item.item && (!target || !existsSync(join(DIST, target)))) {
				report(
					`breadcrumb links ${item.item}, which the build does not contain`,
				);
			}
		});
	}
}

if (pages === 0) {
	console.error(`[schema] no HTML under ${DIST}. Run pnpm build first.`);
	process.exit(1);
}
if (problems.length > 0) {
	console.error(
		`[schema] ${problems.length} problem(s) across ${pages} pages:`,
	);
	for (const problem of problems) console.error(`  ${problem}`);
	process.exit(1);
}
console.log(
	`[schema] ${pages} pages: one parseable graph each, every node type present and every reference resolved.`,
);
