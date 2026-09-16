#!/usr/bin/env node
/**
 * Gates that no prose table overflows the column it sits in, on a phone.
 *
 * THE FAILURE IT EXISTS FOR, measured on the built site on 2026-09-16: every
 * one of the 98 tables this site publishes, in both languages, overflowed its
 * column at a 360 px and at a 400 px viewport, by 176 px to 417 px. The
 * documentation was unreadable sideways on a phone and no gate here noticed,
 * because every other check reads the markup and none of them measure a
 * layout: the HTML was valid, the twins matched, pa11y passed, and the table
 * still needed 674 px of horizontal scroll inside a 328 px screen.
 *
 * `styles/tables.css` fixed it by stacking a table into one block per row
 * below the width its shape needs, and that treatment's whole justification is
 * a measurement. A measurement that lives in somebody's terminal is a
 * measurement that is true once. This is the same walk, committed: build,
 * serve, and put a real browser in front of every page that carries a table,
 * at the two widths a phone actually has.
 *
 * It measures the layout and nothing else. Whether a table stacks or stays a
 * table at a given width is a judgement tables.css makes per column count, and
 * both answers are correct as long as the result fits; the counts are printed
 * so that a change of judgement is visible, not so that it fails.
 *
 * A missing dist/, a corpus with no pages and a corpus with no tables are all
 * failures, not passes. "Every one of the 0 tables fits" is exactly how a path
 * drift, a wrong working directory or a plugin that stopped wrapping tables
 * would read as success.
 *
 * The same walk checks the compact form an index table gets (see "The
 * compact form" in styles/tables.css), and it checks it from the SOURCE, not
 * from the markup. The tables a page declares in its frontmatter
 * (`indexTables`) are read here straight from src/content/docs, and every one
 * of them has to render compact at both widths, on its page, in both
 * languages. Reading the markup instead would have checked only the tables the
 * plugin had marked, so a plugin that stopped marking them (Astro moving the
 * frontmatter away from where src/lib/rehype-tables.mjs reads it, say) would
 * have printed "0 compact indexes" and passed with every index back in stacked
 * boxes. For the same reason a corpus that declares no index at all is a
 * failure. Every `#` link in a table must also land on something on its page.
 *
 * There used to be one more rule here: the layouts pages declared an index of
 * thirteen rows, each linking to that layout's own section, and a row without
 * its link was a failure. Those pages carry no index any more. Each layout's
 * family, motion, width and default fields are stated in its own section, from
 * the registry (src/components/LayoutFacts.astro), because reaching the first
 * card meant scrolling past all thirteen of them on a phone. A set of routes
 * that matches no page is a gate that passes over nothing, so the rule went
 * with the table rather than staying behind, empty, to be believed.
 *
 * The compact form is switched on by an empty custom property,
 * `--table-stacked: ;`, and a minifier that decided an empty value was a
 * mistake is the other way the indexes go back to boxes while every table
 * still fits. This catches that too.
 *
 * The fixtures at the bottom run on every invocation.
 *
 * Usage:
 *   node scripts/check-table-fit.mjs [dist-directory]
 *   node scripts/check-table-fit.mjs --self-test   # fixtures only, no corpus
 */
import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import { parse } from "yaml";

import { freePort, startPreview } from "./preview.mjs";

/** The widths a phone has. Kept to two so the gate stays quick. */
const WIDTHS = [360, 400];

/**
 * A pixel of slack. Layout reads back as fractional and the comparison is
 * between two rounded numbers, so a table exactly filling its column can
 * measure one pixel over. Two pixels is already a visible scrollbar.
 */
const TOLERANCE = 1;

/** Where the pages, and the indexTables they declare, are written. */
const DOCS = resolve(
	dirname(fileURLToPath(import.meta.url)),
	"..",
	"src",
	"content",
	"docs",
);

/** What the plugin wraps every prose table in, and what a page is scanned for. */
const WRAPPER = "table-scroll";

/** Every built page, as paths relative to dist. */
function* pageFiles(dir, prefix = "") {
	for (const entry of readdirSync(dir, { withFileTypes: true })) {
		const relative = prefix ? `${prefix}/${entry.name}` : entry.name;
		if (entry.isDirectory()) yield* pageFiles(join(dir, entry.name), relative);
		else if (entry.isFile() && entry.name === "index.html") yield relative;
	}
}

/**
 * The route a built page is served at, from its path inside dist.
 *
 * @param {string} file a dist-relative path ending in index.html
 * @returns {string} the route, "" for the home page, otherwise trailing-slashed
 */
export function routeOf(file) {
	return file
		.split(sep)
		.join("/")
		.replace(/index\.html$/, "");
}

/**
 * The route a source page is served at, from its path inside src/content/docs.
 *
 * @param {string} file a docs-relative path ending in .md or .mdx
 * @returns {string} the route, "" for the home page, otherwise trailing-slashed
 */
export function routeOfSource(file) {
	const path = file
		.split(sep)
		.join("/")
		.replace(/\.mdx?$/, "")
		.replace(/(^|\/)index$/, "");
	return path ? `${path}/` : "";
}

/**
 * The first-column headings a page's frontmatter declares as index tables.
 *
 * @param {string} source the whole page, frontmatter included
 * @returns {string[]}
 */
export function declaredIndexTables(source) {
	const match = /^---\r?\n([\s\S]*?)\r?\n---/.exec(source);
	if (!match) return [];
	const declared = parse(match[1])?.indexTables;
	return Array.isArray(declared) ? declared.map(String) : [];
}

/** Every page under src/content/docs that declares an index, by route. */
function declaredCorpus(dir = DOCS) {
	const declared = new Map();
	const visit = (folder) => {
		for (const entry of readdirSync(folder, { withFileTypes: true })) {
			const path = join(folder, entry.name);
			if (entry.isDirectory()) visit(path);
			else if (/\.mdx?$/.test(entry.name)) {
				const headings = declaredIndexTables(readFileSync(path, "utf8"));
				if (headings.length > 0) {
					declared.set(routeOfSource(relative(dir, path)), headings);
				}
			}
		}
	};
	visit(dir);
	return declared;
}

/**
 * What is wrong with one page's index tables at one width, read against what
 * its source declared rather than against what the markup says.
 *
 * @param {{
 *   declared: string[],
 *   tables: { firstHeading: string, compact: boolean }[],
 * }} page
 * @returns {string[]} one sentence per problem, none when the page is right
 */
export function indexProblems({ declared, tables }) {
	const problems = [];
	for (const heading of declared) {
		const matching = tables.filter((table) => table.firstHeading === heading);
		if (matching.length === 0) {
			problems.push(
				`declares "${heading}" an index and renders no table with that first column`,
			);
		}
		for (const table of matching) {
			if (!table.compact) {
				problems.push(
					`"${heading}" is declared an index and did not render compact: ` +
						"either src/lib/rehype-tables.mjs did not mark it or the " +
						"compact form in src/styles/tables.css did not switch on",
				);
			}
		}
	}
	return problems;
}

/**
 * Whether one measurement is an overflow, and by how much.
 *
 * @param {{ container: number, content: number }} measured
 * @returns {{ overflow: number, fits: boolean }}
 */
export function verdict({ container, content }) {
	const overflow = content - container;
	return { overflow, fits: overflow <= TOLERANCE };
}

/**
 * Measures every table on the page the browser currently has open. This one
 * runs inside the browser, not in node, so it reaches nothing above it.
 */
function measureTables() {
	return [...document.querySelectorAll(".sl-markdown-content table")].map(
		(table, index) => {
			const wrapper = table.closest(".table-scroll") ?? table.parentElement;
			const heads = [...table.querySelectorAll("thead th")];
			const widths = heads.map((th) =>
				Math.round(th.getBoundingClientRect().width),
			);
			return {
				index,
				container: Math.round(wrapper.clientWidth),
				content: Math.round(Math.max(table.scrollWidth, wrapper.scrollWidth)),
				widest: widths.length
					? heads[widths.indexOf(Math.max(...widths))].textContent.trim()
					: "",
				columns: heads.length,
				stacked: getComputedStyle(table).display !== "table",
				firstHeading: heads.length ? heads[0].textContent.trim() : "",
				// Compact is the stacked table whose second cell runs inline.
				compact:
					getComputedStyle(table).display !== "table" &&
					[...table.querySelectorAll("tbody td:nth-child(2)")].every(
						(cell) => getComputedStyle(cell).display === "inline",
					),
				deadLinks: [...table.querySelectorAll('a[href^="#"]')]
					.filter(
						(link) =>
							!document.getElementById(decodeURIComponent(link.hash.slice(1))),
					)
					.map((link) => link.getAttribute("href")),
			};
		},
	);
}

/** Walks the corpus. Returns the failures and what was looked at. */
async function walk(dist) {
	const files = [...pageFiles(dist)];
	if (files.length === 0) {
		throw new Error(`no built page under ${dist}. Run pnpm build first.`);
	}
	// Only the pages that carry a table are visited: two thirds of this site's
	// pages have none, and a browser is the expensive part of this check.
	const routes = files
		.filter((file) => readFileSync(join(dist, file), "utf8").includes(WRAPPER))
		.map(routeOf)
		.sort();
	if (routes.length === 0) {
		throw new Error(
			`none of the ${files.length} built pages under ${dist} carries a ` +
				`.${WRAPPER} wrapper. Either the corpus lost every table or ` +
				"src/lib/rehype-tables.mjs stopped wrapping them.",
		);
	}

	const declared = declaredCorpus();
	if (declared.size === 0) {
		throw new Error(
			`no page under ${DOCS} declares indexTables, so the compact form ` +
				"would go unchecked. Either the frontmatter key was renamed or " +
				"this file reads the wrong directory.",
		);
	}
	const undeclaredRoutes = [...declared.keys()].filter(
		(route) => !routes.includes(route),
	);
	if (undeclaredRoutes.length > 0) {
		throw new Error(
			`${undeclaredRoutes.map((route) => `/${route}`).join(", ")} declare ` +
				"indexTables and render no table at all.",
		);
	}

	const { chromium } = await import("playwright");
	const preview = startPreview(await freePort());
	const failures = [];
	let tables = 0;
	let stacked = 0;
	let compact = 0;
	let indexes = 0;
	let browser;
	try {
		const base = await preview.announced;
		browser = await chromium.launch();
		for (const width of WIDTHS) {
			const page = await browser.newPage({ viewport: { width, height: 900 } });
			for (const route of routes) {
				const url = new URL(
					`${base.pathname.replace(/\/$/, "")}/${route}`,
					base,
				);
				const response = await page.goto(url.href, { waitUntil: "load" });
				if (!response || !response.ok()) {
					throw new Error(
						`the preview answered ${response ? response.status() : "nothing"} for ${url.pathname}`,
					);
				}
				const measuredTables = await page.evaluate(measureTables);
				if (declared.has(route)) {
					indexes += declared.get(route).length;
					for (const problem of indexProblems({
						declared: declared.get(route),
						tables: measuredTables,
					})) {
						failures.push({ width, route, index: "index", problem });
					}
				}
				for (const measured of measuredTables) {
					tables += 1;
					if (measured.stacked) stacked += 1;
					if (measured.compact) compact += 1;
					const { overflow, fits } = verdict(measured);
					const problems = [];
					if (!fits) {
						problems.push(
							`+${overflow}px (column ${measured.container}px, table ${measured.content}px), ` +
								`${measured.columns} columns, widest "${measured.widest}"`,
						);
					}
					if (measured.deadLinks.length > 0) {
						problems.push(
							`links to ${measured.deadLinks.join(", ")}, which nothing on the page carries`,
						);
					}
					for (const problem of problems) {
						failures.push({ width, route, index: measured.index, problem });
					}
				}
			}
			await page.close();
		}
	} finally {
		if (browser) await browser.close();
		preview.stop();
	}
	if (tables === 0) {
		throw new Error(
			`the ${routes.length} pages scanned rendered no table at all.`,
		);
	}
	return { failures, tables, stacked, compact, indexes, routes: routes.length };
}

/* ------------------------------------------------------------------
 * FIXTURES
 * ------------------------------------------------------------------ */

function selfTest() {
	const failed = [];
	const is = (what, got, want) => {
		if (JSON.stringify(got) !== JSON.stringify(want)) {
			failed.push(
				`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`,
			);
		}
	};

	is("the home page is the empty route", routeOf("index.html"), "");
	is(
		"a page keeps its trailing slash",
		routeOf("api/cost/index.html"),
		"api/cost/",
	);
	is(
		"a Spanish page keeps its prefix",
		routeOf(join("es", "collectors", "measurements", "index.html")),
		"es/collectors/measurements/",
	);

	is(
		"a table inside its column fits",
		verdict({ container: 328, content: 328 }),
		{ overflow: 0, fits: true },
	);
	is(
		"a table narrower than its column fits",
		verdict({ container: 616, content: 540 }),
		{ overflow: -76, fits: true },
	);
	is(
		"one pixel over is slack, not a failure",
		verdict({ container: 328, content: 329 }),
		{ overflow: 1, fits: true },
	);
	is(
		"two pixels over is a failure",
		verdict({ container: 328, content: 330 }),
		{ overflow: 2, fits: false },
	);
	is(
		"the overflow this treatment was written for",
		verdict({ container: 328, content: 674 }),
		{ overflow: 346, fits: false },
	);

	is(
		"a page's source path gives its route",
		routeOfSource(join("es", "card", "layouts.mdx")),
		"es/card/layouts/",
	);
	is("a section index is its folder", routeOfSource("how/index.mdx"), "how/");
	is("the home page is the empty route", routeOfSource("index.mdx"), "");
	is(
		"a block list in the frontmatter is read",
		declaredIndexTables("---\ntitle: x\nindexTables:\n  - Layout\n---\n\nbody"),
		["Layout"],
	);
	is(
		"a page without the key declares nothing",
		declaredIndexTables("---\ntitle: x\n---\n"),
		[],
	);
	const index = { firstHeading: "Store", compact: true };
	is(
		"a compact index is right",
		indexProblems({ declared: ["Store"], tables: [index] }),
		[],
	);
	is(
		"an index the plugin stopped marking is caught from its source",
		indexProblems({
			declared: ["Store"],
			tables: [{ ...index, compact: false }],
		}).length,
		1,
	);
	is(
		"a declared index with no table of that name is caught",
		indexProblems({ declared: ["Store"], tables: [] }).length,
		1,
	);
	is(
		"a table that is not the declared index is not asked to be compact",
		indexProblems({
			declared: ["Store"],
			tables: [index, { firstHeading: "Field", compact: false }],
		}),
		[],
	);

	if (failed.length > 0) {
		console.error("[table-fit] fixtures failed:");
		for (const line of failed) console.error(`  ${line}`);
		process.exit(1);
	}
	return failed.length;
}

/* ------------------------------------------------------------------
 * RUN
 * ------------------------------------------------------------------ */

selfTest();
if (process.argv.includes("--self-test")) {
	console.log("[table-fit] fixtures pass.");
	process.exit(0);
}

const DIST = resolve(
	process.argv.slice(2).find((argument) => !argument.startsWith("--")) ??
		"dist",
);

try {
	const { failures, tables, stacked, compact, indexes, routes } =
		await walk(DIST);
	if (failures.length > 0) {
		console.error(
			`[table-fit] ${failures.length} problems over ${tables} measurements:`,
		);
		for (const failure of failures) {
			console.error(
				`  ${failure.width}px /${failure.route} table ${failure.index}: ${failure.problem}`,
			);
		}
		console.error(
			"[table-fit] a table that does not fit has to scroll sideways, which " +
				"on a phone is how the documentation stopped being readable. Either " +
				"the breakpoint in src/styles/tables.css no longer covers this " +
				"shape, or something in the cell cannot wrap. An index in stacked " +
				"boxes is the compact form of the same file and of " +
				"src/lib/rehype-tables.mjs; a link that lands nowhere is the " +
				"page's own.",
		);
		process.exit(1);
	}
	console.log(
		`[table-fit] ${tables} measurements over ${routes} pages at ` +
			`${WIDTHS.join(" and ")} px: every table fits its column ` +
			`(${stacked - compact} stacked, ${compact} compact indexes, ` +
			`${tables - stacked} still tables); all ${indexes} measurements of ` +
			"the index tables the sources declare rendered compact, and every " +
			"row link lands.",
	);
} catch (error) {
	console.error(`[table-fit] ${error.message}`);
	process.exit(1);
}
