#!/usr/bin/env node
/**
 * Writes docs/ from the site's English pages.
 *
 * The repository used to carry two complete sets of documentation: 74 pages
 * under site/src/content/docs and ten hand-written files under docs/, covering
 * the same ground. They were edited in the same commits, which is how they
 * drifted in both directions at once: the plain-Markdown measurement table ran
 * 54 fields ahead of the site's, the site's troubleshooting page answered six
 * questions the plain one did not and lacked four it had, and the only
 * explanation of the Parquet file limit lived where nothing published it.
 * Neither copy was wrong enough to look wrong, which is the point: a stale
 * document is still a valid document, and nothing fails.
 *
 * So there is one source now. The pages are the source, docs/ is output, and
 * --check is what makes that true rather than aspirational. The reduction from
 * MDX to Markdown is the same function the site's own markdown twins are built
 * from (src/lib/page-markdown.mjs), so the twin a reader fetches from the site
 * and the file a reader opens on GitHub cannot come to say different things.
 *
 * What this adds on top of the twin, and why:
 *
 *   - Several pages per file. docs/sinks.md is the ten pages under /sinks/,
 *     docs/running.md the four under /install/. The names are the ones the
 *     repository already links to, from CLAUDE.md and from the README, so the
 *     mapping is a manifest here rather than one file per page.
 *   - Links into the site become absolute. A page writes /ghchronicle/sinks/
 *     because it is served from there; the same text in a file read on GitHub
 *     would be a link to GitHub's own root.
 *   - Images become repository-relative. The built page serves a hashed,
 *     re-encoded asset; docs/ points at the source file, which is in the same
 *     checkout the reader already has.
 *   - Headings drop one level where a file holds more than one page, so the
 *     page's own title is the section heading.
 *
 * Every English page must be claimed by exactly one entry of MANIFEST or named
 * in NOT_IN_DOCS with a reason. A page added to the site therefore fails this
 * check until somebody decides where it belongs, which is the only way docs/
 * stays complete without anyone remembering it exists.
 *
 * Usage:
 *   node scripts/gen-docs.mjs           # write docs/
 *   node scripts/gen-docs.mjs --check   # fail if docs/ is stale
 */
import { readFileSync, readdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import { parseDocument } from "yaml";

const SITE = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const REPO = path.dirname(SITE);
const CONTENT = path.join(SITE, "src/content/docs");
const DOCS = path.join(REPO, "docs");

// The origin and the base path as astro.config.mjs states them, read out of it
// rather than restated here. src/lib/site.mjs builds every URL from these two,
// and inside the build it reads them from Astro; this script has no
// import.meta.env, so it passes the same two values through the environment
// before importing anything that reads them.
const config = readFileSync(path.join(SITE, "astro.config.mjs"), "utf8");
/** @param {RegExp} pattern @param {string} what @returns {string} */
function fromConfig(pattern, what) {
	const match = pattern.exec(config);
	if (!match) throw new Error(`astro.config.mjs declares no ${what}`);
	return match[1];
}
process.env.SITE = fromConfig(/\n\tsite:\s*"([^"]+)"/, "site");
process.env.BASE_URL = fromConfig(
	/const siteBase\s*=\s*"([^"]+)"/,
	"base path",
);

const { reduceBody } = await import("../src/lib/page-markdown.mjs");
const { BASE, routeOf } = await import("../src/lib/site.mjs");

// The origin these files advertise, which is the canonical domain rather than
// the one the pages are served from; astro.config.mjs says why beside the
// declaration. `site.mjs` keeps building the served URLs for the site itself,
// so only the links that leave the site are rewritten here.
const PUBLIC = fromConfig(
	/const publicDocs\s*=\s*"([^"]+)"/,
	"public docs URL",
);

/** @param {string} route @returns {string} the advertised URL of a page. */
const publicUrl = (route) => (route ? `${PUBLIC}/${route}/` : `${PUBLIC}/`);

// docs/<file> <- the site pages it holds, in the order they are read in.
//
// The titles are the ones these files already carried: a file that several
// pages land in needs a name of its own, and renaming them would break the
// links the repository already makes to them.
const MANIFEST = [
	{
		file: "getting-started.md",
		title: "Getting started",
		routes: ["start", "start/quickstart", "start/token"],
	},
	{
		file: "how-it-works.md",
		title: "How it works",
		routes: ["how", "how/dating", "how/backfill"],
	},
	{
		file: "rate-limits.md",
		title: "Rate limits",
		routes: ["api", "api/cost", "api/limits"],
	},
	{
		file: "running.md",
		title: "Running it",
		routes: [
			"install",
			"install/linux",
			"install/macos",
			"install/windows",
			"install/systemd",
			"install/docker",
			"install/actions",
		],
	},
	{
		file: "configuration.md",
		title: "Configuration",
		routes: [
			"configuration",
			"configuration/targets",
			"configuration/cadences",
			"configuration/logging",
		],
	},
	{
		file: "metrics.md",
		title: "Metrics",
		routes: ["collectors", "collectors/measurements"],
	},
	{
		file: "sinks.md",
		title: "Sinks",
		routes: [
			"sinks",
			"sinks/influxdb",
			"sinks/prometheus",
			"sinks/otlp",
			"sinks/postgres",
			"sinks/graphite",
			"sinks/elasticsearch",
			"sinks/loki",
			"sinks/telegraf",
			"sinks/file",
		],
	},
	{
		file: "dashboards.md",
		title: "Dashboards",
		routes: ["dashboards", "dashboards/panels"],
	},
	{
		file: "card.md",
		title: "The one-shot card",
		routes: ["card", "card/layouts"],
	},
	{ file: "cli.md", title: "The command line", routes: ["reference/cli"] },
	{ file: "subprocess.md", routes: ["reference/subprocess"] },
	{ file: "troubleshooting.md", routes: ["reference/troubleshooting"] },
	{ file: "testing.md", routes: ["reference/testing"] },
];

// The one page that is not documentation. Its copy lives in a typed object
// rather than in prose (src/data/home.ts), the repository's own README opens
// with the same pitch, and a landing reduced to Markdown is a list of links to
// the pages docs/ already holds.
const NOT_IN_DOCS = new Map([
	[
		"",
		"the landing page: the README opens with the same pitch, and the rest of it is links to pages docs/ already holds",
	],
]);

// Named beside the generated files in the index, because the index is the
// first page of docs/ and these two are where a reader is sent next. The
// sentences are the ones docs/README.md carried before it was generated.
const NEIGHBOURS = [
	[
		"../config.example.yaml",
		"Every setting, with a comment beside it on what it does",
	],
	[
		"../dashboards/README.md",
		"Importing and regenerating the five Grafana dashboards, one per store",
	],
];

/**
 * The line that tells a reader who arrives here that this is output, and what
 * to edit instead. It names the source, because a file that says only "do not
 * edit" leaves the reader with nowhere to go.
 *
 * @param {string[]} sources the page files this one is generated from
 * @returns {string} the notice
 */
const notice = (sources) =>
	`Generated by \`site/scripts/gen-docs.mjs\` from ${
		sources.length === 0
			? "the pages of the documentation site"
			: sources.length === 1
				? `\`${sources[0]}\``
				: "the pages of the documentation site named under each heading below"
	}: do not edit this file. Change the page, and its Spanish twin beside it, then run \`make docs\`.`;

/**
 * Every English page of the content collection, keyed by route.
 *
 * @returns {Map<string, { route: string, title: string, description: string, body: string, file: string, dir: string }>}
 */
function readPages() {
	/** @type {Map<string, any>} */
	const pages = new Map();
	/** @param {string} dir */
	function walk(dir) {
		for (const entry of readdirSync(dir, { withFileTypes: true })) {
			const full = path.join(dir, entry.name);
			if (entry.isDirectory()) {
				// The Spanish mirror is a translation of these same pages. docs/ is
				// English, as everything in the repository outside site/…/es is.
				if (path.relative(CONTENT, full) !== "es") walk(full);
				continue;
			}
			if (!entry.name.endsWith(".mdx") && !entry.name.endsWith(".md")) continue;
			const relative = path.relative(CONTENT, full).replaceAll(path.sep, "/");
			const source = readFileSync(full, "utf8");
			const split = /^---[ \t]*\r?\n([\s\S]*?)\r?\n---[ \t]*(?:\r?\n|$)/.exec(
				source,
			);
			if (!split) throw new Error(`${relative}: no frontmatter`);
			const front = parseDocument(split[1]).toJS() ?? {};
			if (!front.title) throw new Error(`${relative}: no frontmatter title`);
			if (!front.description) {
				throw new Error(`${relative}: no frontmatter description`);
			}
			pages.set(routeOf(relative), {
				route: routeOf(relative),
				title: String(front.title),
				description: String(front.description),
				body: source.slice(split[0].length),
				file: `site/src/content/docs/${relative}`,
				dir: path.dirname(full),
				splash: front.template === "splash",
			});
		}
	}
	walk(CONTENT);
	return pages;
}

/**
 * Rewrites one link or image target for a file read from the repository rather
 * than served from the site.
 *
 * @param {string} target the target as the page writes it
 * @param {boolean} isImage whether it came from an image
 * @param {{ dir: string, route: string }} context the page it was written in
 * @returns {string} the target as docs/ should carry it
 */
function retarget(target, isImage, context) {
	// An image served from somewhere else is not a file in this checkout, and
	// resolving it against the page's directory would make it one: a directory
	// called https: with the host under it.
	if (SCHEME.test(target)) return target;
	if (isImage || target.startsWith(".")) {
		// An asset the page reaches by walking up out of the content tree. The
		// built page serves a hashed copy of it; docs/ points at the file itself,
		// which the reader already has in the same checkout.
		const absolute = path.resolve(context.dir, target);
		return path.relative(DOCS, absolute).replaceAll(path.sep, "/");
	}
	// A heading on the page itself. Several pages land in one file here, so a
	// heading is no longer the only one of its name: six section names occur
	// twice in docs/metrics.md, and GitHub resolves #account to the first of
	// them, which is the wrong section. The anchor is exact on the page it was
	// written for, so that is where it points.
	if (target.startsWith("#")) return `${publicUrl(context.route)}${target}`;
	// A link into the site. Rooted at the base path because that is where the
	// page is served from; read from GitHub the same text is a link to GitHub.
	if (target.startsWith(`${BASE}/`)) {
		return `${PUBLIC}${target.slice(BASE.length)}`;
	}
	return target;
}

// A scheme, which is what separates a target somewhere else from a file in
// this checkout.
const SCHEME = /^[a-z][a-z0-9+.-]*:/i;

// A link or an image, with its optional title: [text](target "title").
//
// The text may wrap. These pages are wrapped at eighty columns, and a link
// whose text broke across a line kept the site-rooted target every other one
// lost, which read from a file on GitHub is a link to GitHub's own root. A
// blank line ends the text, because that is a paragraph and not a link.
const TARGET =
	/(!?)(\[(?:[^\]\n]|\n(?![ \t]*\n))*\])\(([^)\s]+)((?:\s+"[^"]*")?)\)/g;

/**
 * One heading of a page that lands in a file with others, dropped a level so
 * the page's own title is the section above it.
 *
 * @param {string} line
 * @param {{ file: string }} context
 * @returns {string} the line
 */
function demote(line, context) {
	return line.replace(/^(#{1,6})(\s)/, (_, hashes, space) => {
		if (hashes.length >= 6) {
			throw new Error(
				`${context.file}: a level ${hashes.length} heading cannot drop a level. ` +
					"Give this page a file of its own in site/scripts/gen-docs.mjs.",
			);
		}
		return `#${hashes}${space}`;
	});
}

/**
 * One run of prose with every link and image in it retargeted, and then held
 * to the result.
 *
 * A target still rooted at the site's base path is a link to GitHub's own
 * root, and one still naming a heading is a link to whichever section of the
 * file happens to carry that name first. Neither fails anything downstream:
 * check-doc-links reads a rooted target as somebody else's business, and an
 * anchor resolves, to the wrong place. So they fail here.
 *
 * @param {string} prose a run of lines with no fenced code in it
 * @param {{ dir: string, file: string, route: string }} context
 * @returns {string} the prose
 */
function rewrite(prose, context) {
	const out = prose.replaceAll(
		TARGET,
		(_, bang, text, target, title) =>
			`${bang}${text}(${retarget(target, bang === "!", context)}${title})`,
	);
	const missed = out.includes("](#")
		? "a heading"
		: out.includes(`](${BASE}/`)
			? `${BASE}/`
			: "";
	if (missed) {
		throw new Error(
			`${context.file}: a link to ${missed} survived the rewrite, so docs/ would ` +
				"carry a target that resolves nowhere. Its text is shaped in a way " +
				"site/scripts/gen-docs.mjs does not read.",
		);
	}
	return out;
}

/**
 * The body of one page as docs/ carries it: headings dropped a level where the
 * file holds more than one page, links and images retargeted.
 *
 * Fenced code is left exactly as it is, at whatever indentation it sits at:
 * <Steps> and <TabItem> both reduce to list items, so most of the code in
 * these pages is four or more columns in. Half of these pages open a fence
 * with a shell comment on its own line, which is not a heading, and several
 * carry filesystem paths under /var/lib/ghchronicle, which are not links.
 *
 * @param {string} markdown the reduced page body
 * @param {{ demote: boolean, dir: string, file: string, route: string }} context
 * @returns {string} the body as it belongs in docs/
 */
function transplant(markdown, context) {
	/** @type {string | null} */
	let fence = null;
	/** @type {string[]} */
	const out = [];
	/** @type {string[]} */
	let prose = [];
	const flush = () => {
		if (prose.length > 0) out.push(rewrite(prose.join("\n"), context));
		prose = [];
	};
	for (const line of markdown.split("\n")) {
		const marker = /^[ \t]*(`{3,}|~{3,})/.exec(line);
		if (fence) {
			out.push(line);
			if (
				marker &&
				marker[1][0] === fence[0] &&
				marker[1].length >= fence.length
			) {
				fence = null;
			}
			continue;
		}
		if (marker) {
			flush();
			out.push(line);
			fence = marker[1];
			continue;
		}
		prose.push(context.demote ? demote(line, context) : line);
	}
	flush();
	return out.join("\n");
}

/**
 * One file of docs/.
 *
 * @param {{ file: string, title?: string, routes: string[] }} spec
 * @param {Map<string, any>} pages every English page
 * @returns {string} the file
 */
function render(spec, pages) {
	const many = spec.routes.length > 1;
	const first = pages.get(spec.routes[0]);
	const parts = [
		`# ${spec.title ?? first.title}`,
		"",
		notice(spec.routes.map((route) => pages.get(route).file)),
	];
	for (const route of spec.routes) {
		const page = pages.get(route);
		if (many) parts.push("", `## ${page.title}`);
		parts.push(
			"",
			page.description,
			"",
			// An autolink rather than the bare URL the twin carries: docs/ is
			// linted as Markdown, and MD034 is right that a bare URL in prose is
			// not a link everywhere it will be read.
			`Source: <${publicUrl(route)}>`,
			"",
			transplant(
				reduceBody({ body: page.body, file: page.file, locale: "en" }),
				{ demote: many, dir: page.dir, file: page.file, route },
			),
		);
	}
	return `${parts
		.join("\n")
		.replace(/\n{3,}/g, "\n\n")
		.trim()}\n`;
}

/**
 * The index: one row per generated file, in the order the site's sidebar puts
 * them, saying what its first page says about itself.
 *
 * @param {Map<string, any>} pages every English page
 * @returns {string} docs/README.md
 */
function renderIndex(pages) {
	const rows = MANIFEST.map((spec) => {
		const first = pages.get(spec.routes[0]);
		return `| [${spec.title ?? first.title}](${spec.file}) | ${first.description} |`;
	});
	return `${[
		"# Documentation",
		"",
		notice([]),
		"",
		"| Page | What it covers |",
		"| --- | --- |",
		...rows,
		"",
		"Beside them, in the repository rather than on the site:",
		"",
		...NEIGHBOURS.map(([target, what]) => `- [${target}](${target}): ${what}`),
	].join("\n")}\n`;
}

const pages = readPages();

const claimed = new Map();
for (const spec of MANIFEST) {
	for (const route of spec.routes) {
		if (!pages.has(route)) {
			throw new Error(
				`site/scripts/gen-docs.mjs claims a page that does not exist: ${route}`,
			);
		}
		if (claimed.has(route)) {
			throw new Error(
				`${route} is claimed by both docs/${claimed.get(route)} and docs/${spec.file}`,
			);
		}
		claimed.set(route, spec.file);
	}
}
const orphans = [...pages.keys()].filter(
	(route) => !claimed.has(route) && !NOT_IN_DOCS.has(route),
);
if (orphans.length > 0) {
	console.error(
		`[docs] ${orphans.length} page(s) belong to no file of docs/:\n` +
			orphans.map((route) => `  /${route}/`).join("\n") +
			"\n  Add each to MANIFEST in site/scripts/gen-docs.mjs, or to NOT_IN_DOCS with the reason.",
	);
	process.exit(1);
}

const wanted = new Map([
	...MANIFEST.map((spec) => [spec.file, render(spec, pages)]),
	["README.md", renderIndex(pages)],
]);

const checking = process.argv.includes("--check");
const stale = [];
for (const [file, text] of wanted) {
	const target = path.join(DOCS, file);
	const current = readSafely(target);
	if (current === text) continue;
	if (checking) stale.push(file);
	else writeFileSync(target, text);
}

/** @param {string} target @returns {string} the file, or "" if it is not there. */
function readSafely(target) {
	try {
		return readFileSync(target, "utf8");
	} catch {
		return "";
	}
}

const extra = readdirSync(DOCS)
	.filter((name) => name.endsWith(".md"))
	.filter((name) => !wanted.has(name));
if (extra.length > 0) {
	console.error(
		`[docs] ${extra.length} file(s) under docs/ are generated by nothing:\n` +
			extra.map((name) => `  docs/${name}`).join("\n") +
			"\n  Delete them, or give them an entry in MANIFEST in site/scripts/gen-docs.mjs.",
	);
	process.exit(1);
}

if (checking) {
	if (stale.length > 0) {
		console.error(
			`[docs] ${stale.length} file(s) under docs/ no longer match the pages they are generated from:\n` +
				stale.map((name) => `  docs/${name}`).join("\n") +
				"\n  Refresh them with: make docs",
		);
		process.exit(1);
	}
	console.log(
		`[docs] ${wanted.size} files, generated from ${claimed.size} pages, all up to date.`,
	);
} else {
	console.log(
		`[docs] wrote ${wanted.size} files from ${claimed.size} pages under site/src/content/docs.`,
	);
}
