// The JSON-LD graph the site publishes, read back out of the build.
//
// It is the one part of a page written for machines alone, and until this
// check existed nothing in the repository read it. html-validate does not look
// inside a <script>, htmlhint does not either, pa11y has no reason to, and a
// link checker sees no links there. A graph could stop being JSON, lose the
// node that carries the author, or point every reference at an @id that is not
// in the document, and every other check would stay green.
//
// What it asserts is derived from each page's own URL, from the build around
// it and from the repository, not from what src/components/overrides/Head.astro
// believes: a check that asked the emitter what it emits would agree with the
// emitter's defects. So the version comes from VERSION and CHANGELOG.md, held
// to the release tags where this checkout has them, a page's first publication
// from git, its section from the sidebar the build rendered, its translation
// from the source tree, its speakable selectors from its own HTML, and its
// FAQPage from the headings and paragraphs the build rendered.
//
// It needs the repository's full history: a page's datePublished is the day
// git first saw its file, and a shallow clone cannot say. docs.yml checks out
// with `fetch-depth: 0`.
//
// Usage: node scripts/check-structured-data.mjs [dist-directory]
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { join, resolve } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import { parseRelease } from "../src/lib/release.mjs";
import { version } from "../src/lib/remark-version.mjs";

const DIST = resolve(process.argv[2] ?? "dist");
const SITE_DIR = fileURLToPath(new URL("..", import.meta.url));
const REPO_ROOT = resolve(SITE_DIR, "..");
const DOCS = join(SITE_DIR, "src", "content", "docs");
const PERSON_ID = "https://jmrp.io/#person";
const RELEASE = parseRelease(
	version,
	readFileSync(join(REPO_ROOT, "CHANGELOG.md"), "utf8"),
);

// The nodes every page carries. They describe one website, one program and one
// person, so they must be the same bytes on every page: a consumer merging two
// pages by @id must never be handed two values for one property.
const SHARED_TYPES = [
	"Person",
	"WebSite",
	"SoftwareApplication",
	"SoftwareSourceCode",
];
const WIKIDATA_ENTITY = /^http:\/\/www\.wikidata\.org\/entity\/Q\d+$/;

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

/** Standard output of a git command in the repository, or "" if it fails. */
function git(args) {
	try {
		return execFileSync("git", args, {
			cwd: REPO_ROOT,
			encoding: "utf8",
			stdio: ["ignore", "pipe", "ignore"],
		}).trim();
	} catch {
		return "";
	}
}

const shallow = git(["rev-parse", "--is-shallow-repository"]);
const fullHistory = shallow === "false";
if (!fullHistory) {
	problems.push(
		shallow === "true"
			? "the checkout is shallow, so no page's datePublished can be checked against git; check out with fetch-depth: 0"
			: "git could not be run here, so no page's datePublished can be checked against it",
	);
}
// The day the history begins, in the committer's own timezone, which is how
// CHANGELOG.md dates a release: the first commit is the public 1.0.0.
const ROOT_DAY = git(["log", "--max-parents=0", "--format=%cs", "HEAD"])
	.split("\n")
	.filter(Boolean)
	.at(-1);
// The day the current release's tag was made, when this checkout has it. The
// site deploys on the merge that bumps VERSION and the tag follows, so a
// missing tag is the normal state for those minutes and is not reported.
const TAG_DAY =
	git([
		"for-each-ref",
		"--format=%(taggerdate:short)",
		`refs/tags/${RELEASE.tag}`,
	]) || undefined;

function* htmlFiles(dir, prefix = "") {
	for (const entry of readdirSync(dir, { withFileTypes: true })) {
		const relative = prefix ? `${prefix}/${entry.name}` : entry.name;
		if (entry.isDirectory()) yield* htmlFiles(join(dir, entry.name), relative);
		else if (entry.isFile() && entry.name.endsWith(".html")) yield relative;
	}
}

/** The dist path a site URL addresses, or undefined if it is not on this site. */
function distPathOf(url) {
	if (typeof url !== "string" || !url.startsWith(SITE_ROOT)) return undefined;
	const relative = url.slice(SITE_ROOT.length).replace(/^\//, "");
	return relative === "" || relative.endsWith("/")
		? `${relative}index.html`
		: relative;
}

/** Whether a site URL names a file the build contains. */
const built = (url) => {
	const path = distPathOf(url);
	return Boolean(path) && existsSync(join(DIST, path));
};

/** The source file behind a route ("" for the English home), if one exists. */
function sourceOf(route) {
	const candidates =
		route === ""
			? ["index.mdx", "index.md"]
			: [
					`${route}.mdx`,
					`${route}.md`,
					`${route}/index.mdx`,
					`${route}/index.md`,
				];
	return candidates.map((file) => join(DOCS, file)).find(existsSync);
}

/** The route of the same page in the other language. */
const otherLanguage = (route) =>
	route === "es"
		? ""
		: route.startsWith("es/")
			? route.slice(3)
			: route === ""
				? "es"
				: `es/${route}`;

/** When git first saw a file, following renames: the committer date of the oldest add. */
function firstAdded(file) {
	const oldest = git([
		"log",
		"--follow",
		"--diff-filter=A",
		"--format=%cI",
		"--",
		file,
	])
		.split("\n")
		.filter(Boolean)
		.at(-1);
	return oldest ? new Date(oldest).toISOString() : undefined;
}

const decode = (text) =>
	text
		.replace(/&#(\d+);/g, (_, code) => String.fromCodePoint(Number(code)))
		.replace(/&#x([\da-f]+);/gi, (_, code) =>
			String.fromCodePoint(Number.parseInt(code, 16)),
		)
		.replace(/&quot;/g, '"')
		.replace(/&lt;/g, "<")
		.replace(/&gt;/g, ">")
		.replace(/&amp;/g, "&");

/**
 * The label of the sidebar group around the link the build marked as the
 * current page, or undefined when the sidebar does not list the page. Read
 * from the rendered sidebar, which is what a reader sees, rather than from the
 * configuration, which is what the emitter reads.
 */
function sidebarSection(html) {
	const start = html.indexOf('id="starlight__sidebar"');
	const end = html.indexOf("</sl-sidebar-pane>", start);
	if (start === -1 || end === -1) return undefined;
	// Each group is a <details> whose <summary> holds its label; a link outside
	// every group has no section.
	const groups = [];
	for (const [token, label] of html
		.slice(start, end)
		.matchAll(
			/<details\b|<\/details>|<span class="large[^"]*">([^<]*)<\/span>|<a\s[^>]*aria-current="page"/g,
		)) {
		if (token.startsWith("<details")) groups.push(undefined);
		else if (token === "</details>") groups.pop();
		else if (label !== undefined) {
			if (groups.length > 0) groups[groups.length - 1] = decode(label);
		} else return groups.at(-1);
	}
	return undefined;
}

// Enough of an HTML parser to evaluate a speakable selector against the page
// it names. The build's HTML is Astro's own output and html-validate holds it
// to the standard, so explicit end tags, void elements and SVG's self-closing
// tags are all it meets; script and style are skipped as raw text.
const VOID = new Set(
	"area base br col embed hr img input link meta source track wbr".split(" "),
);
const RAW_TEXT = new Set(["script", "style", "textarea", "title"]);

/** The page as a tree: elements with a name, classes, children and parent. */
function parseHtml(html) {
	const root = { name: "#root", classes: [], children: [], parent: undefined };
	let current = root;
	const tokens =
		/<!--[\s\S]*?-->|<![^>]*>|<\/([a-zA-Z][\w:-]*)\s*>|<([a-zA-Z][\w:-]*)((?:[^>"']|"[^"]*"|'[^']*')*)>|([^<]+)|</g;
	let match;
	while ((match = tokens.exec(html))) {
		const [token, closing, opening, attrs, text] = match;
		if (text !== undefined) {
			current.children.push(text);
		} else if (closing) {
			const name = closing.toLowerCase();
			let open = current;
			while (open !== root && open.name !== name) open = open.parent;
			if (open !== root) current = open.parent;
		} else if (opening) {
			const name = opening.toLowerCase();
			const element = {
				name,
				classes: (/\sclass="([^"]*)"/.exec(attrs)?.[1] ?? "")
					.split(/\s+/)
					.filter(Boolean),
				children: [],
				parent: current,
			};
			current.children.push(element);
			if (RAW_TEXT.has(name)) {
				const end = html.indexOf(`</${name}`, tokens.lastIndex);
				const stop = end === -1 ? html.length : end;
				element.children.push(html.slice(tokens.lastIndex, stop));
				tokens.lastIndex = stop;
				// The end tag is consumed by the next iteration, which pops back.
				current = element;
			} else if (!VOID.has(name) && !token.endsWith("/>")) {
				current = element;
			}
		}
	}
	return root;
}

const textOf = (node) =>
	typeof node === "string" ? node : node.children.map(textOf).join("");

/** The element after this one among its parent's children, text skipped. */
const nextElement = (element) => {
	const siblings = element.parent.children.filter(
		(child) => typeof child !== "string",
	);
	return siblings[siblings.indexOf(element) + 1];
};

/**
 * Text as the source and the page can both be compared in: whitespace
 * collapsed, and the curly quotes and ellipsis smartypants sets on the page
 * taken back to the straight characters the source is written in.
 */
const comparable = (text) =>
	text
		.replace(/[\u2018\u2019]/g, "'")
		.replace(/[\u201C\u201D]/g, '"')
		.replace(/\u2026/g, "...")
		.replace(/\s+/g, " ")
		.trim();

/** Where two texts part, with a little of each, for a report. */
function divergence(expected, actual) {
	let at = 0;
	while (at < expected.length && expected[at] === actual[at]) at++;
	const from = Math.max(0, at - 20);
	return `at character ${at + 1}: the page has ${JSON.stringify(expected.slice(from, at + 40))}, the FAQPage ${JSON.stringify(actual.slice(from, at + 40))}`;
}

/** Whether a source file's frontmatter sets `faq: true`. */
const setsFaq = (file) =>
	/^faq:\s*true\s*$/m.test(
		/^---\n([\s\S]*?)\n---/.exec(readFileSync(file, "utf8"))?.[1] ?? "",
	);

/**
 * The FAQPage against the page it describes. src/lib/faq.mjs builds it from the
 * MDX source without MDX, and refuses the markup it knows it cannot reduce;
 * this reads the other side, what the build rendered, so a reduction and a
 * renderer that come to disagree are caught whichever of them moved. Each
 * question has to be an h2 of the page's content, word for word, and its
 * answer the paragraph that comes right after that h2; every h2 that asks a
 * question has to be in the FAQPage.
 */
function checkFaq(faq, tree, document, report) {
	if (document && faq.isPartOf?.["@id"] !== document["@id"]) {
		report(
			`the FAQPage is part of ${JSON.stringify(faq.isPartOf)}, expected the page's own ${document["@type"]}`,
		);
	}
	if (document && faq.url !== document.url) {
		report(`the FAQPage says url ${faq.url}, the page ${document.url}`);
	}
	// Starlight wraps a heading and its anchor link in a div, and the paragraph
	// under the heading follows that div.
	const headings = new Map();
	for (const heading of select(tree, ".sl-markdown-content h2") ?? []) {
		const block = heading.parent.classes.includes("sl-heading-wrapper")
			? heading.parent
			: heading;
		const next = nextElement(block);
		headings.set(
			comparable(decode(textOf(heading))),
			next?.name === "p" ? comparable(decode(textOf(next))) : undefined,
		);
	}
	const questions = Array.isArray(faq.mainEntity) ? faq.mainEntity : [];
	if (questions.length === 0) report("the FAQPage names no Question");
	const asked = new Set();
	for (const [index, question] of questions.entries()) {
		const name = comparable(String(question?.name ?? ""));
		const answer = comparable(String(question?.acceptedAnswer?.text ?? ""));
		if (
			question?.["@type"] !== "Question" ||
			question.acceptedAnswer?.["@type"] !== "Answer" ||
			!name ||
			!answer
		) {
			report(
				`FAQ entry ${index + 1} is not a Question with a name and an Answer with text`,
			);
			continue;
		}
		if (asked.has(name)) {
			report(`the FAQPage asks ${JSON.stringify(name)} twice`);
			continue;
		}
		asked.add(name);
		if (!headings.has(name)) {
			report(
				`the FAQPage asks ${JSON.stringify(name)}, which is not an h2 of the page`,
			);
		} else if (headings.get(name) === undefined) {
			report(
				`the h2 ${JSON.stringify(name)} is not followed by a paragraph, so no text on the page is the answer the FAQPage gives`,
			);
		} else if (headings.get(name) !== answer) {
			report(
				`the FAQPage's answer to ${JSON.stringify(name)} is not the paragraph under that h2, ${divergence(headings.get(name), answer)}`,
			);
		}
	}
	for (const name of headings.keys()) {
		if (name.endsWith("?") && !asked.has(name)) {
			report(
				`the h2 ${JSON.stringify(name)} asks a question the FAQPage leaves out`,
			);
		}
	}
	return questions.length;
}

/**
 * The elements a selector matches, for the subset a speakable selector here is
 * written in: type and class compounds, `:first-of-type`, and the descendant
 * and child combinators. Anything else returns undefined, so a selector this
 * cannot read is reported rather than passed.
 */
function select(root, selector) {
	const parts = selector.trim().split(/\s*(>)\s*|\s+/);
	const steps = [];
	for (let index = 0; index < parts.length; index += 2) {
		const compound = /^([a-z][\w-]*)?((?:\.[\w-]+)*)(:first-of-type)?$/i.exec(
			parts[index],
		);
		if (!compound || (!compound[1] && !compound[2])) return undefined;
		steps.push({
			name: compound[1]?.toLowerCase(),
			classes: compound[2].split(".").filter(Boolean),
			firstOfType: Boolean(compound[3]),
			// How this step relates to the one before it.
			combinator: index === 0 ? undefined : (parts[index - 1] ?? " "),
		});
	}
	const is = (element, step) =>
		(!step.name || element.name === step.name) &&
		step.classes.every((name) => element.classes.includes(name)) &&
		(!step.firstOfType ||
			element.parent.children.find(
				(sibling) =>
					typeof sibling !== "string" && sibling.name === element.name,
			) === element);
	const matches = (element, depth) => {
		if (!is(element, steps[depth])) return false;
		if (depth === 0) return true;
		if (steps[depth].combinator === ">") {
			return (
				element.parent !== root &&
				Boolean(element.parent) &&
				matches(element.parent, depth - 1)
			);
		}
		for (let up = element.parent; up && up !== root; up = up.parent) {
			if (matches(up, depth - 1)) return true;
		}
		return false;
	};
	const found = [];
	const walk = (node) => {
		for (const child of node.children) {
			if (typeof child === "string") continue;
			if (matches(child, steps.length - 1)) found.push(child);
			walk(child);
		}
	};
	walk(root);
	return found;
}

/**
 * A text property both languages must carry: `[{"@value", "@language": "en"},
 * {"@value", "@language": "es"}]`, each non-empty.
 */
function bilingual(value, what, report) {
	const values = Array.isArray(value) ? value : [];
	const byLanguage = Object.fromEntries(
		values.map((item) => [item?.["@language"], item?.["@value"]]),
	);
	const languages = values.map((item) => item?.["@language"]).sort();
	if (
		languages.join() !== "en,es" ||
		!byLanguage.en?.trim() ||
		!byLanguage.es?.trim()
	) {
		report(
			`${what} is ${JSON.stringify(value)}, expected one non-empty value tagged "en" and one tagged "es"`,
		);
	}
}

/**
 * The nodes every page shares, checked once: the identity check below holds
 * every other page to the same bytes.
 */
function checkSharedNodes(nodes, report) {
	const website = nodes.find((node) => node["@type"] === "WebSite");
	const software = nodes.find(
		(node) => node["@type"] === "SoftwareApplication",
	);
	const source = nodes.find((node) => node["@type"] === "SoftwareSourceCode");

	if (website) bilingual(website.description, "WebSite description", report);

	if (software) {
		if (software.softwareVersion !== RELEASE.version) {
			report(
				`softwareVersion is ${JSON.stringify(software.softwareVersion)}, but VERSION is ${RELEASE.version}`,
			);
		}
		if (
			software.releaseNotes !== `${software.url}/releases/tag/${RELEASE.tag}`
		) {
			report(
				`releaseNotes is ${JSON.stringify(software.releaseNotes)}, expected the ${RELEASE.tag} release page of ${software.url}`,
			);
		}
		if (software.dateModified !== RELEASE.date) {
			report(
				`the software's dateModified is ${JSON.stringify(software.dateModified)}, but CHANGELOG.md dates ${RELEASE.version} ${RELEASE.date}`,
			);
		}
		if (TAG_DAY && TAG_DAY !== RELEASE.date) {
			report(
				`CHANGELOG.md dates ${RELEASE.version} ${RELEASE.date}, but the tag ${RELEASE.tag} was made on ${TAG_DAY}`,
			);
		}
		if (software.datePublished !== RELEASE.firstDate) {
			report(
				`the software's datePublished is ${JSON.stringify(software.datePublished)}, but the oldest release in CHANGELOG.md is dated ${RELEASE.firstDate}`,
			);
		}
		// A shallow clone's first commit is its cut-off, not the public 1.0.0,
		// and the shallow checkout is reported once already.
		if (fullHistory && ROOT_DAY !== RELEASE.firstDate) {
			report(
				`the oldest release in CHANGELOG.md is dated ${RELEASE.firstDate}, but the repository's first commit, the public 1.0.0, is from ${ROOT_DAY}`,
			);
		}
		if (
			typeof software.downloadUrl !== "string" ||
			!software.downloadUrl.startsWith(`${software.url}/releases`)
		) {
			report(
				`downloadUrl ${JSON.stringify(software.downloadUrl)} is not a releases page of ${software.url}`,
			);
		}
		if (!built(software.installUrl)) {
			report(
				`installUrl ${JSON.stringify(software.installUrl)} is not a page the build contains`,
			);
		}
		const sameAs = software.sameAs;
		if (
			!Array.isArray(sameAs) ||
			sameAs.length === 0 ||
			new Set(sameAs).size !== sameAs.length ||
			sameAs.some(
				(url) =>
					typeof url !== "string" ||
					!url.startsWith("https://") ||
					url === software.url,
			)
		) {
			report(
				`the software's sameAs is ${JSON.stringify(sameAs)}, expected distinct https URLs other than its own url`,
			);
		}
		const alternateNames = [software.alternateName ?? []].flat();
		if (!alternateNames.length || alternateNames.some((name) => !name)) {
			report("the software has no alternateName");
		}
		if (!software.applicationSubCategory) {
			report("the software has no applicationSubCategory");
		}
		bilingual(software.description, "the software's description", report);
		bilingual(
			software.disambiguatingDescription,
			"the software's disambiguatingDescription",
			report,
		);
	}

	if (source) {
		const language = source.programmingLanguage;
		if (
			language?.["@type"] !== "ComputerLanguage" ||
			language?.name !== "Go" ||
			!WIKIDATA_ENTITY.test(language?.["@id"] ?? "")
		) {
			report(
				`programmingLanguage is ${JSON.stringify(language)}, expected a ComputerLanguage named Go whose @id is its Wikidata entity`,
			);
		}
		// The binary is static: a runtime platform there claims a dependency the
		// program does not have.
		if ("runtimePlatform" in source) {
			report(
				`the source code states runtimePlatform ${JSON.stringify(source.runtimePlatform)}, and the binary needs no runtime`,
			);
		}
		if (!software || source.targetProduct?.["@id"] !== software["@id"]) {
			report(
				`the source code's targetProduct is ${JSON.stringify(source.targetProduct)}, expected the software node`,
			);
		}
	}
}

const pages = [];
const shared = new Map();
let sharedChecked = false;
let faqPages = 0;
let faqQuestions = 0;
for (const file of htmlFiles(DIST)) {
	const html = readFileSync(join(DIST, file), "utf8");
	const page = `/${file.replace(/index\.html$/, "")}`;
	const report = (message) => problems.push(`${page}: ${message}`);

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
		...SHARED_TYPES,
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
	// reference to anything else is a pointer no consumer can resolve. A node
	// that stands for something outside this document (the page's translation,
	// the programming language) is embedded with its type and name, so it is
	// not a bare reference and does not match here.
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

	// The shared nodes: checked in full on the first page, then held to the
	// same bytes everywhere else.
	for (const type of SHARED_TYPES) {
		const node = nodes.find((candidate) => candidate["@type"] === type);
		if (!node) continue;
		const bytes = JSON.stringify(node);
		if (!shared.has(type)) shared.set(type, { bytes, page });
		else if (shared.get(type).bytes !== bytes) {
			report(
				`its ${type} node differs from the one on ${shared.get(type).page}`,
			);
		}
	}
	if (!sharedChecked) {
		checkSharedNodes(nodes, report);
		sharedChecked = true;
	}

	const route = page.replace(/^\/|\/$/g, "");
	const locale = route === "es" || route.startsWith("es/") ? "es" : "en";
	const document = nodes.find(
		(node) => node["@type"] === "TechArticle" || node["@type"] === "WebPage",
	);
	const translated =
		!isNotFound &&
		Boolean(sourceOf(route)) &&
		Boolean(sourceOf(otherLanguage(route)));
	// A Spanish URL with no Spanish source is Starlight's fallback, which
	// renders the English page's file and so carries that file's dates and
	// frontmatter.
	const sourceFile =
		sourceOf(route) ??
		(locale === "es" ? sourceOf(otherLanguage(route)) : undefined);
	const tree = parseHtml(html);

	if (document) {
		const canonical = `${new URL(SITE_ROOT).origin}${BASE_PATH}${page}`;
		if (document.url !== canonical) {
			report(
				`the document node says url ${document.url}, expected ${canonical}`,
			);
		}
		// The twin it announces has to exist, or the cheap representation is a 404.
		const twin = document.encoding?.contentUrl;
		if (!built(twin)) {
			report(
				`announces its markdown at ${twin}, which the build does not contain`,
			);
		}

		if (isHome) {
			const software = nodes.find(
				(node) => node["@type"] === "SoftwareApplication",
			);
			if (!software || document.mainEntity?.["@id"] !== software["@id"]) {
				report(
					`the landing's mainEntity is ${JSON.stringify(document.mainEntity)}, expected the software node`,
				);
			}
		} else {
			const section = sidebarSection(html);
			if (document.articleSection !== section) {
				report(
					`articleSection is ${JSON.stringify(document.articleSection)}, but the sidebar puts the page under ${JSON.stringify(section)}`,
				);
			}
		}

		// Every selector has to name something a voice can read on this page.
		// One that matches nothing is not an error to any validator; it is a
		// page that silently has nothing to say.
		const speakable = document.speakable;
		const selectors = speakable?.cssSelector;
		if (
			speakable?.["@type"] !== "SpeakableSpecification" ||
			!Array.isArray(selectors) ||
			selectors.length === 0
		) {
			report(
				`speakable is ${JSON.stringify(speakable)}, expected a SpeakableSpecification with a list of cssSelector`,
			);
		} else {
			for (const selector of selectors) {
				const found = select(tree, selector);
				if (!found) {
					report(
						`speakable selector ${JSON.stringify(selector)} is beyond what this check can evaluate; teach select() or simplify it`,
					);
				} else if (!found.some((element) => textOf(element).trim())) {
					report(
						`speakable selector ${JSON.stringify(selector)} matches no element with text on this page`,
					);
				}
			}
		}

		// Published is the day git first saw the page's file, and a file git has
		// never seen (a page not committed yet) claims no publication at all.
		// Modified can only follow published.
		const { datePublished, dateModified } = document;
		if (fullHistory) {
			const added = sourceFile ? firstAdded(sourceFile) : undefined;
			if (datePublished !== added) {
				report(
					added
						? `datePublished is ${JSON.stringify(datePublished)}, but git first added ${sourceFile.slice(SITE_DIR.length)} on ${added}`
						: `datePublished is ${JSON.stringify(datePublished)}, but git has no commit that added the page's file`,
				);
			}
		}
		if (
			datePublished &&
			dateModified &&
			!(new Date(datePublished) <= new Date(dateModified))
		) {
			report(
				`datePublished ${datePublished} is after dateModified ${dateModified}`,
			);
		}
	}

	// The trail Head.astro emits on every document page: positions 1..n in
	// order, a name on every crumb, and no crumb linking a page the build lacks,
	// since one crumb pointing at a 404 invalidates the whole trail. A crumb may
	// be a name without a link, which is the documented form for a section with
	// no index page.
	const breadcrumb = nodes.find((node) => node["@type"] === "BreadcrumbList");
	if (breadcrumb) {
		(breadcrumb.itemListElement ?? []).forEach((item, index) => {
			if (item.position !== index + 1) {
				report(`breadcrumb item ${index + 1} is at position ${item.position}`);
			}
			if (!item.name) report(`breadcrumb item ${index + 1} has no name`);
			if (item.item && !built(item.item)) {
				report(
					`breadcrumb links ${item.item}, which the build does not contain`,
				);
			}
		});
	}

	// A page whose source sets `faq` carries one FAQPage, and no other page
	// carries any.
	const faqs = nodes.filter((node) => node["@type"] === "FAQPage");
	const wantsFaq = !isNotFound && Boolean(sourceFile) && setsFaq(sourceFile);
	if (wantsFaq && faqs.length !== 1) {
		report(
			`${sourceFile.slice(SITE_DIR.length)} sets faq, but the graph has ${faqs.length} FAQPage nodes`,
		);
	} else if (!wantsFaq && faqs.length > 0) {
		report("the graph has a FAQPage, but the page's source does not set faq");
	}
	for (const faq of faqs) {
		faqQuestions += checkFaq(faq, tree, document, report);
		faqPages++;
	}

	pages.push({ page, route, locale, document, translated, report });
}

// Translations: an English page whose Spanish source exists names it with
// workTranslation, and the Spanish page names the English one back with
// translationOfWork. Each side is checked against the other side's own node,
// so a URL, id, title or language that does not match what the other page
// says about itself is caught, and so is a link to a page the build lacks.
const byRoute = new Map(pages.map((entry) => [entry.route, entry]));
for (const { route, locale, document, translated, report } of pages) {
	if (!document) continue;
	const property = locale === "es" ? "translationOfWork" : "workTranslation";
	const reverse = locale === "es" ? "workTranslation" : "translationOfWork";
	const link = document[property];
	if (document[reverse]) {
		report(`carries ${reverse}, which belongs on the other language's page`);
	}
	if (!translated) {
		if (link) {
			report(`names a translation at ${link.url}, but no source for it exists`);
		}
		continue;
	}
	const other = byRoute.get(otherLanguage(route));
	if (!link) {
		report(`has a translation, but no ${property}`);
		continue;
	}
	if (!other?.document || !built(link.url)) {
		report(
			`${property} names ${link.url}, which is not a document the build contains`,
		);
		continue;
	}
	const target = other.document;
	for (const key of ["@type", "@id", "url", "inLanguage"]) {
		if (link[key] !== target[key]) {
			report(
				`${property} says ${key} ${JSON.stringify(link[key])}, but that page says ${JSON.stringify(target[key])}`,
			);
		}
	}
	const titleKey = target["@type"] === "WebPage" ? "name" : "headline";
	if (link[titleKey] !== target[titleKey]) {
		report(
			`${property} says ${titleKey} ${JSON.stringify(link[titleKey])}, but that page says ${JSON.stringify(target[titleKey])}`,
		);
	}
	if (target[reverse]?.["@id"] !== document["@id"]) {
		report(
			`${property} names ${link.url}, which does not name this page back with ${reverse}`,
		);
	}
}

if (pages.length === 0) {
	console.error(`[schema] no HTML under ${DIST}. Run pnpm build first.`);
	process.exit(1);
}
if (problems.length > 0) {
	console.error(
		`[schema] ${problems.length} problem(s) across ${pages.length} pages:`,
	);
	for (const problem of problems) console.error(`  ${problem}`);
	process.exit(1);
}
const translatedPairs = pages.filter(
	(entry) => entry.locale === "en" && entry.translated,
).length;
console.log(
	`[schema] ${pages.length} pages: one parseable graph each, every node type present and every reference resolved; ` +
		`the shared nodes identical on every page, softwareVersion ${RELEASE.version} as VERSION says, ` +
		`${translatedPairs} translation pairs naming each other, every section the sidebar's, ` +
		"every speakable selector reading text on its page, every datePublished the day git first saw the page, " +
		`and ${faqQuestions} FAQ questions on ${faqPages} pages each an h2 of its page answered by the paragraph under it.`,
);
