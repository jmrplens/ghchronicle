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
 * Two more things are read off the same walk, for the compact form an index
 * table gets (see "The compact form" in styles/tables.css). A table its page
 * declared an index must actually be compact wherever it is not wide: that
 * form is switched on by an empty custom property, `--table-stacked: ;`, and
 * a minifier that decided an empty value was a mistake would put every index
 * back into stacked boxes while every table still fit. And every link the
 * plugin wrote from a row to a section of its page must land on an element
 * that exists, because the plugin derives the anchor before the heading ids
 * are written and cannot check it itself.
 *
 * The fixtures at the bottom run on every invocation.
 *
 * Usage:
 *   node scripts/check-table-fit.mjs [dist-directory]
 *   node scripts/check-table-fit.mjs --self-test   # fixtures only, no corpus
 */
import { readFileSync, readdirSync } from "node:fs";
import { join, resolve, sep } from "node:path";
import process from "node:process";

import { freePort, startPreview } from "./preview.mjs";

/** The widths a phone has. Kept to two so the gate stays quick. */
const WIDTHS = [360, 400];

/**
 * A pixel of slack. Layout reads back as fractional and the comparison is
 * between two rounded numbers, so a table exactly filling its column can
 * measure one pixel over. Two pixels is already a visible scrollbar.
 */
const TOLERANCE = 1;

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
				declaredIndex: wrapper.dataset.form === "index",
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

	const { chromium } = await import("playwright");
	const preview = startPreview(await freePort());
	const failures = [];
	let tables = 0;
	let stacked = 0;
	let compact = 0;
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
				for (const measured of await page.evaluate(measureTables)) {
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
					if (!indexFormHolds(measured)) {
						problems.push(
							"declared an index, stacked, and not compact: the compact " +
								"form in src/styles/tables.css did not switch on",
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
	return { failures, tables, stacked, compact, routes: routes.length };
}

/**
 * Whether a table in the form it measured in is the form its page asked for:
 * an index that is not a table any more has to be the compact index, not the
 * stacked blocks. Every other table, and an index still wide, passes.
 *
 * @param {{ declaredIndex: boolean, stacked: boolean, compact: boolean }} measured
 * @returns {boolean}
 */
export function indexFormHolds({ declaredIndex, stacked, compact }) {
	return !declaredIndex || !stacked || compact;
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
		"an index still wide is what its column allows",
		indexFormHolds({ declaredIndex: true, stacked: false, compact: false }),
		true,
	);
	is(
		"an index below its breakpoint is compact",
		indexFormHolds({ declaredIndex: true, stacked: true, compact: true }),
		true,
	);
	is(
		"an index in stacked boxes is the failure the form exists for",
		indexFormHolds({ declaredIndex: true, stacked: true, compact: false }),
		false,
	);
	is(
		"a reference table stacks and is not asked to be compact",
		indexFormHolds({ declaredIndex: false, stacked: true, compact: false }),
		true,
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
	const { failures, tables, stacked, compact, routes } = await walk(DIST);
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
				"boxes, or a row linking nowhere, is the compact form of the same " +
				"file and of src/lib/rehype-tables.mjs.",
		);
		process.exit(1);
	}
	console.log(
		`[table-fit] ${tables} measurements over ${routes} pages at ` +
			`${WIDTHS.join(" and ")} px: every table fits its column ` +
			`(${stacked - compact} stacked, ${compact} compact indexes, ` +
			`${tables - stacked} still tables), and every row link lands.`,
	);
} catch (error) {
	console.error(`[table-fit] ${error.message}`);
	process.exit(1);
}
