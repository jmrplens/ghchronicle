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
 * The root size a key's break points are checked at: 200 % text, which is the
 * size accessibility guidance is written around and the size at which the
 * widest names of this corpus stop fitting their card. Checked in the page the
 * gate has already loaded, by setting the root and putting it back, so it
 * costs one layout rather than a second pass over the corpus.
 */
const LARGE_ROOT = 24;

/**
 * The separators an identifier of this corpus is built from. A break after one
 * of them is a break the name carries; the same list is in
 * src/lib/rehype-tables.mjs, which writes a `wbr` after each.
 */
const KEY_SEPARATORS = new Set([
	".",
	"_",
	"-",
	":",
	"=",
	"/",
	",",
	";",
	"@",
	"|",
]);

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

/**
 * The `{table, default}` pairs a page's frontmatter declares.
 *
 * @param {string} source the whole page, frontmatter included
 * @returns {{ table: string, default: string }[]}
 */
export function declaredDefaultColumns(source) {
	const match = /^---\r?\n([\s\S]*?)\r?\n---/.exec(source);
	if (!match) return [];
	const declared = parse(match[1])?.defaultColumns;
	return Array.isArray(declared)
		? declared.map((entry) => ({
				table: String(entry.table),
				default: String(entry.default),
			}))
		: [];
}

/**
 * Every page under `dir` that declares at least one entry of `frontmatterKey`
 * (`indexTables` or `defaultColumns`), by route, read with `reader`.
 *
 * @param {(source: string) => unknown[]} reader
 * @param {string} dir
 * @returns {Map<string, unknown[]>}
 */
function declaredBy(reader, dir = DOCS) {
	const declared = new Map();
	const visit = (folder) => {
		for (const entry of readdirSync(folder, { withFileTypes: true })) {
			const path = join(folder, entry.name);
			if (entry.isDirectory()) visit(path);
			else if (/\.mdx?$/.test(entry.name)) {
				const values = reader(readFileSync(path, "utf8"));
				if (values.length > 0) {
					declared.set(routeOfSource(relative(dir, path)), values);
				}
			}
		}
	};
	visit(dir);
	return declared;
}

/** Every page under src/content/docs that declares an index, by route. */
function declaredCorpus(dir = DOCS) {
	return declaredBy(declaredIndexTables, dir);
}

/** Every page under src/content/docs that declares a default column, by route. */
function declaredDefaultCorpus(dir = DOCS) {
	return declaredBy(declaredDefaultColumns, dir);
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
 * The share of a declared column's rows that has to reach the reader beside
 * its key. Below this the entry is not describing the table any more.
 *
 * Half, because half is the tightest floor this corpus allows: the smallest
 * legitimate ratio in it is exactly one of two, `configuration/`'s dedupe
 * table, where `sinks.dedupe_horizon`'s `720h` hoists and
 * `sinks.dedupe_file`'s sentence keeps its own labelled block. A column most
 * of whose rows print a labelled block is not a default column, it is an
 * ordinary column with some defaults in it, and the declaration should go
 * rather than render half a table one way and half the other.
 */
const HOIST_FLOOR = 0.5;

/**
 * Whether a cell's text is a VALUE rather than a sentence: one unbroken run of
 * characters with no space in it.
 *
 * Deliberately a second implementation of `isValue()` in
 * src/lib/rehype-tables.mjs rather than an import of it: what this gate has to
 * notice is the plugin marking something OTHER than what the rule says, and a
 * checker that asks the plugin what it marked can only ever agree with it.
 *
 * @param {string} text
 * @returns {boolean}
 */
export function isValueText(text) {
	const trimmed = text.trim();
	return trimmed.length > 0 && !/\s/.test(trimmed);
}

/**
 * How much of one declared column actually hoisted, counted in CELLS.
 *
 * `hasDefaultCell` only says that the column was not lost entirely, which is
 * what a per-cell rule made too weak to gate on: thirteen of fourteen defaults
 * can stop hoisting and the last one still answers it. This recounts the whole
 * column from the rendered page: how many rows it has, how many of them hold a
 * value by the rule above, and how many carry the mark.
 *
 * @param {{ headings?: string[], columnCells?: { text: string, marked: boolean }[][] }} table
 * @param {string} defaultHeading the column heading the page declared
 * @returns {{
 *   rows: number,
 *   values: number,
 *   hoisted: number,
 *   unmarked: string[],
 *   sentences: string[],
 * } | null} null when this table was not measured cell by cell
 */
export function hoistCensus(table, defaultHeading) {
	const column = (table.headings ?? []).indexOf(defaultHeading);
	const cells = column === -1 ? undefined : table.columnCells?.[column];
	if (!cells) return null;
	return {
		rows: cells.length,
		values: cells.filter((cell) => isValueText(cell.text)).length,
		hoisted: cells.filter((cell) => cell.marked).length,
		unmarked: cells
			.filter((cell) => isValueText(cell.text) && !cell.marked)
			.map((cell) => cell.text),
		sentences: cells
			.filter((cell) => cell.marked && !isValueText(cell.text))
			.map((cell) => cell.text),
	};
}

/**
 * What is wrong with one page's default-column tables at one width, read
 * against what its source declared rather than against what the markup says.
 *
 * Five things can be wrong: the cell was never marked, it was marked and did
 * not land beside the key, it landed there and the line does not fit (which is
 * either half of that line taking a second line box), the column marked
 * something other than what the rule says, or too little of the column reached
 * the reader beside its key at all.
 *
 * The last two are counted in cells, not in declarations. A declared column
 * that hoists one row of fourteen satisfies every per-table question that can
 * be asked of it, which is how a column can quietly stop working.
 *
 * @param {{
 *   declared: { table: string, default: string }[],
 *   tables: {
 *     firstHeading: string,
 *     defaultForm: boolean,
 *     hasDefaultCell: boolean,
 *     headings?: string[],
 *     columnCells?: { text: string, marked: boolean }[][],
 *     keyWraps?: { text: string, lines: number }[],
 *     valueWraps?: { text: string, lines: number }[],
 *   }[],
 * }} page
 * @returns {string[]} one sentence per problem, none when the page is right
 */
export function defaultColumnProblems({ declared, tables }) {
	const problems = [];
	for (const { table: heading, default: defaultHeading } of declared) {
		const matching = tables.filter((table) => table.firstHeading === heading);
		if (matching.length === 0) {
			problems.push(
				`declares "${heading}" a default-column table (default "${defaultHeading}") ` +
					"and renders no table with that first column",
			);
		}
		for (const table of matching) {
			if (!table.hasDefaultCell) {
				problems.push(
					`"${heading}" declares "${defaultHeading}" as its default column, and ` +
						'no cell of it carries data-role="default": either ' +
						"src/lib/rehype-tables.mjs did not mark it or the column heading " +
						"changed without the frontmatter following it",
				);
			} else if (!table.defaultForm) {
				problems.push(
					`"${heading}"'s default column did not render beside the label: ` +
						"either src/lib/rehype-tables.mjs did not mark the cell or the " +
						"default form in src/styles/tables.css did not switch on",
				);
			}
			for (const { text, lines } of table.keyWraps ?? []) {
				problems.push(
					`"${heading}"'s key ${text} took ${lines} line boxes: a row's ` +
						"name broken mid-word is what this form has to keep from " +
						"happening. The hoisted default beside it is taking room the " +
						"key needs, so lower --table-default-value-maxw in " +
						"src/styles/tables.css, or leave that default in its own block",
				);
			}
			for (const { text, lines } of table.valueWraps ?? []) {
				problems.push(
					`"${heading}"'s hoisted default "${text}" took ${lines} line ` +
						"boxes: a value beside the label has to read as one value on " +
						"one line. Either --table-default-value-maxw in " +
						"src/styles/tables.css is tighter than the widest value this " +
						"corpus hoists, or this default is long enough to belong in " +
						"its own labelled block",
				);
			}
			const census = hoistCensus(table, defaultHeading);
			if (!census) continue;
			if (census.unmarked.length > 0) {
				problems.push(
					`"${heading}" hoisted ${census.hoisted} of the ${census.values} ` +
						`cells of "${defaultHeading}" that are values: ` +
						`${quoted(census.unmarked)} ` +
						`${
							census.unmarked.length === 1
								? "is a value and carries"
								: "are values and carry"
						} no ` +
						'data-role="default". Either isValue() in ' +
						"src/lib/rehype-tables.mjs stopped accepting what it accepted " +
						"(a non-breaking space inside a value reads as a space), or " +
						"something after it dropped the attribute. Every value of a " +
						"declared column has to reach the reader beside its key",
				);
			}
			if (census.sentences.length > 0) {
				problems.push(
					`"${heading}" hoisted ${quoted(census.sentences)}, which ` +
						`${census.sentences.length === 1 ? "has" : "have"} a space in ` +
						`${census.sentences.length === 1 ? "it" : "them"}: a sentence ` +
						"unlabelled in the corner of a card stops reading as a " +
						"default. src/lib/rehype-tables.mjs marks values only, so " +
						"either its rule changed or this mark was not put there by it",
				);
			}
			if (census.rows > 0 && census.hoisted < census.rows * HOIST_FLOOR) {
				problems.push(
					`"${heading}" hoists ${census.hoisted} of its ${census.rows} rows, ` +
						`under the ${Math.ceil(census.rows * HOIST_FLOOR)} this gate ` +
						"asks of a declared column: a column most of whose rows print " +
						"their own labelled block is not a default column. Either " +
						"write those defaults as values (one unbroken run, no space), " +
						`or drop "${heading}" from defaultColumns in the page's ` +
						"frontmatter and let the table stack as any other reference " +
						"table does",
				);
			}
		}
	}
	return problems;
}

/**
 * A few cell texts for a message, quoted, with the tail counted rather than
 * printed: a fourteen-row column that lost every mark should name the shape of
 * the problem, not recite the column.
 *
 * @param {string[]} texts
 * @returns {string}
 */
function quoted(texts) {
	const shown = texts.slice(0, 3).map((text) => `"${text}"`);
	const rest = texts.length - shown.length;
	return rest > 0 ? `${shown.join(", ")} and ${rest} more` : shown.join(", ");
}

/**
 * Whether a line break inside a key landed on a boundary the key carries: a
 * separator, a space, or a camel-case hump (`aB`, and `ABc`, so `SUIDSGID`
 * counts as one word).
 *
 * @param {{ after: string, at: string }} split the character the break follows
 *   and the one it lands on
 * @returns {boolean}
 */
export function breakIsAtBoundary({ after, at }) {
	if (KEY_SEPARATORS.has(after) || /\s/.test(after)) return true;
	if (/[a-z0-9]/.test(after) && /[A-Z]/.test(at)) return true;
	return /[A-Z]/.test(after) && /[A-Z]/.test(at);
}

/**
 * What is wrong with the way a page's keys broke at a large root.
 *
 * Stacked, `td code` carries `overflow-wrap: anywhere`, which is what keeps a
 * long path inside a phone and which, on a name too wide for its card, breaks
 * wherever it runs out of room: `sinks.dedupe_horizo` / `n`.
 * src/lib/rehype-tables.mjs answers that by writing a `wbr` at every boundary
 * the name already carries, and a real break opportunity outranks
 * `overflow-wrap`, so a name that carries one breaks there instead. This is
 * what holds that: at 200 % text, every break in a key that is a whole name
 * has to follow a boundary.
 *
 * @param {{ text: string, breaks: { after: string, at: string }[] }[]} keys
 * @returns {string[]} one sentence per key that broke somewhere else
 */
export function keyBreakProblems(keys) {
	const problems = [];
	for (const { text, breaks } of keys) {
		for (const split of breaks) {
			if (breakIsAtBoundary(split)) continue;
			problems.push(
				`the key ${JSON.stringify(text)} breaks after ` +
					`${JSON.stringify(split.after)} at a ${LARGE_ROOT}px root ` +
					"(200 % text), which is not a boundary it carries. A name may " +
					"break at a separator or a camel-case hump, and " +
					"src/lib/rehype-tables.mjs writes a wbr at each of those; a " +
					"break anywhere else is overflow-wrap: anywhere in " +
					"src/styles/tables.css taking what it can get, which is how a " +
					"row's name reads as sinks.dedupe_horizo / n",
			);
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
			const bodyRows = [...table.querySelectorAll("tbody tr")];
			const defaultCells = bodyRows
				.map((row) => ({
					row,
					key: row.querySelector("td:first-child"),
					value: row.querySelector('td[data-role="default"]'),
				}))
				.filter((entry) => entry.value);
			// How many lines a node's text occupies. Measured over the TEXT
			// nodes under it, because a range over an element's contents also
			// returns the element's own box (a code span's padded box sits a
			// couple of pixels above the text inside it, and counting rects
			// would read one line as two). Tops within half a line of each
			// other are the same line: on one line a code span's 0.875em text
			// and the prose around it do not share a top edge.
			const lineBoxes = (node) => {
				const walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT);
				const tops = [];
				for (let text = walker.nextNode(); text; text = walker.nextNode()) {
					if (!text.nodeValue.trim()) continue;
					const range = document.createRange();
					range.selectNodeContents(text);
					for (const rect of range.getClientRects()) {
						if (rect.width > 0 && rect.height > 0) tops.push(rect.top);
					}
				}
				const leading =
					Number.parseFloat(getComputedStyle(node).lineHeight) || 16;
				return tops
					.sort((a, b) => a - b)
					.filter(
						(top, at, sorted) => at === 0 || top - sorted[at - 1] > leading / 2,
					).length;
			};
			const stackedTable = getComputedStyle(table).display !== "table";
			const wrapped = (nodes) =>
				nodes
					.filter(Boolean)
					.map((node) => ({
						text: node.textContent.trim(),
						lines: lineBoxes(node),
					}))
					.filter((measured) => measured.lines > 1);
			// Every cell of every column of a declared table, in markup order,
			// with whether it carries the hoist mark. Collected only for the
			// tables a page declared (the wrapper says so), because this is
			// the only place a cell-by-cell census is worth its bytes, and
			// read back in node against the checker's own copy of the rule.
			const columnCells =
				wrapper.dataset && wrapper.dataset.form === "default"
					? heads.map((_, column) =>
							bodyRows
								.map((row) => row.querySelectorAll("td")[column])
								.filter(Boolean)
								.map((cell) => ({
									text: cell.textContent.trim(),
									marked: cell.dataset.role === "default",
								})),
						)
					: [];
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
				headings: heads.map((th) => th.textContent.trim()),
				columnCells,
				// Compact is the stacked table whose second cell runs inline.
				compact:
					getComputedStyle(table).display !== "table" &&
					[...table.querySelectorAll("tbody td:nth-child(2)")].every(
						(cell) => getComputedStyle(cell).display === "inline",
					),
				// A default column exists on this table when at least one body cell
				// carries the role. It is IN FORM when every one of those cells sits
				// beside its row's key rather than below it: top edge within a couple
				// of pixels of the key's own top edge, which is only true once the
				// row is the grid src/styles/tables.css switches it into. Below that
				// breakpoint, or if the switch never fired, the cell falls back into
				// normal stacked flow, well below the key.
				hasDefaultCell: defaultCells.length > 0,
				defaultForm:
					getComputedStyle(table).display !== "table" &&
					defaultCells.length > 0 &&
					defaultCells.every(
						({ key, value }) =>
							key &&
							Math.abs(
								value.getBoundingClientRect().top -
									key.getBoundingClientRect().top,
							) <= 2,
					),
				// The two halves of the line the default form makes: the key on
				// the left and the hoisted value on the right. Only measured
				// stacked, the only form in which they share a line.
				keyWraps: stackedTable
					? wrapped(defaultCells.map(({ key }) => key?.querySelector("code")))
					: [],
				valueWraps: stackedTable
					? wrapped(defaultCells.map(({ value }) => value))
					: [],
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

/**
 * Where every key of the page breaks at a large root. Runs inside the browser,
 * in the page the gate has already loaded: it sets the root font size, reads
 * the break points, and puts the root back, so it costs one layout and no
 * second pass over the corpus.
 *
 * Only a cell that IS one name is measured, which is the only cell
 * src/lib/rehype-tables.mjs gives break opportunities to: a cell holding a
 * list of names already breaks between them, and a cell of prose with a code
 * span in it is a sentence whichever way it wraps.
 *
 * @param {number} root the root font size to measure at, in px
 */
function measureKeyBreaks(root) {
	const previous = document.documentElement.style.fontSize;
	document.documentElement.style.fontSize = `${root}px`;
	try {
		const measured = [];
		let keys = 0;
		for (const cell of document.querySelectorAll(
			'.sl-markdown-content td[data-role="identity"]',
		)) {
			const spans = cell.querySelectorAll("code");
			if (spans.length !== 1) continue;
			const code = spans[0];
			if (code.textContent.trim() !== cell.textContent.trim()) continue;
			keys += 1;
			// One client rect per line box while the element is inline, so this
			// is the cheap question "did it wrap at all" asked of every key,
			// before the per-character walk is asked of the few that did.
			if (code.getClientRects().length < 2) continue;
			const walker = document.createTreeWalker(code, NodeFilter.SHOW_TEXT);
			const characters = [];
			for (let node = walker.nextNode(); node; node = walker.nextNode()) {
				for (let at = 0; at < node.nodeValue.length; at += 1) {
					const range = document.createRange();
					range.setStart(node, at);
					range.setEnd(node, at + 1);
					const rect = range.getClientRects()[0];
					characters.push({
						character: node.nodeValue[at],
						top: rect ? rect.top : null,
					});
				}
			}
			const leading =
				Number.parseFloat(getComputedStyle(code).lineHeight) || root;
			const breaks = [];
			let previousTop = null;
			for (let at = 0; at < characters.length; at += 1) {
				const { top } = characters[at];
				if (top === null) continue;
				if (previousTop !== null && top - previousTop > leading / 2) {
					let before = at - 1;
					while (before >= 0 && characters[before].top === null) before -= 1;
					breaks.push({
						after: before >= 0 ? characters[before].character : "",
						at: characters[at].character,
					});
				}
				previousTop = top;
			}
			if (breaks.length > 0) measured.push({ text: code.textContent, breaks });
		}
		return { keys, measured };
	} finally {
		document.documentElement.style.fontSize = previous;
	}
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

	const declaredDefaults = declaredDefaultCorpus();
	if (declaredDefaults.size === 0) {
		throw new Error(
			`no page under ${DOCS} declares defaultColumns, so the default form ` +
				"would go unchecked. Either the frontmatter key was renamed or " +
				"this file reads the wrong directory.",
		);
	}
	const undeclaredDefaultRoutes = [...declaredDefaults.keys()].filter(
		(route) => !routes.includes(route),
	);
	if (undeclaredDefaultRoutes.length > 0) {
		throw new Error(
			`${undeclaredDefaultRoutes.map((route) => `/${route}`).join(", ")} ` +
				"declare defaultColumns and render no table at all.",
		);
	}

	const { chromium } = await import("playwright");
	const preview = startPreview(await freePort());
	const failures = [];
	let tables = 0;
	let stacked = 0;
	let compact = 0;
	let indexes = 0;
	let defaultColumnsChecked = 0;
	let defaultCellsChecked = 0;
	let defaultRowsChecked = 0;
	let keysChecked = 0;
	let keysWrapped = 0;
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
				// Where the keys break at 200 % text. Only at the first width:
				// the question is the key against its own card, which the
				// stacked form gives it at both, and one layout per page is
				// what keeps this cheap.
				if (width === WIDTHS[0]) {
					const { keys, measured } = await page.evaluate(
						measureKeyBreaks,
						LARGE_ROOT,
					);
					keysChecked += keys;
					keysWrapped += measured.length;
					for (const problem of keyBreakProblems(measured)) {
						failures.push({ width, route, index: "key", problem });
					}
				}
				if (declared.has(route)) {
					indexes += declared.get(route).length;
					for (const problem of indexProblems({
						declared: declared.get(route),
						tables: measuredTables,
					})) {
						failures.push({ width, route, index: "index", problem });
					}
				}
				if (declaredDefaults.has(route)) {
					const declaredHere = declaredDefaults.get(route);
					defaultColumnsChecked += declaredHere.length;
					// Counted in cells, so that the success line states how much
					// of each declared column reached the reader rather than how
					// many columns were named in a frontmatter.
					for (const { table: heading, default: column } of declaredHere) {
						for (const table of measuredTables) {
							if (table.firstHeading !== heading) continue;
							const census = hoistCensus(table, column);
							if (!census) continue;
							defaultCellsChecked += census.hoisted;
							defaultRowsChecked += census.rows;
						}
					}
					for (const problem of defaultColumnProblems({
						declared: declaredHere,
						tables: measuredTables,
					})) {
						failures.push({ width, route, index: "default", problem });
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
	return {
		failures,
		tables,
		stacked,
		compact,
		indexes,
		defaultColumnsChecked,
		defaultCellsChecked,
		defaultRowsChecked,
		keysChecked,
		keysWrapped,
		routes: routes.length,
	};
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

	is(
		"a block list of pairs in the frontmatter is read",
		declaredDefaultColumns(
			"---\ntitle: x\ndefaultColumns:\n  - table: Key\n    default: Default\n---\n\nbody",
		),
		[{ table: "Key", default: "Default" }],
	);
	is(
		"a page without the key declares no default columns",
		declaredDefaultColumns("---\ntitle: x\n---\n"),
		[],
	);
	const github = {
		firstHeading: "Key",
		hasDefaultCell: true,
		defaultForm: true,
	};
	is(
		"a default column beside its label is right",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [github],
		}),
		[],
	);
	is(
		"a default column the plugin stopped marking is caught from its source",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [{ ...github, hasDefaultCell: false, defaultForm: false }],
		}).length,
		1,
	);
	is(
		"a default column the CSS stopped switching on is caught",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [{ ...github, defaultForm: false }],
		}).length,
		1,
	);
	is(
		"a declared default column with no table of that name is caught",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [],
		}).length,
		1,
	);
	is(
		"a key broken onto a second line box is caught",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [
				{ ...github, keyWraps: [{ text: "sinks.dedupe_file", lines: 2 }] },
			],
		}).length,
		1,
	);
	is(
		"a hoisted default that wrapped is caught",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [
				{ ...github, valueWraps: [{ text: "api.github.com", lines: 2 }] },
			],
		}).length,
		1,
	);
	is(
		"a table with neither half wrapped is right",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [{ ...github, keyWraps: [], valueWraps: [] }],
		}),
		[],
	);

	is(
		"a single run of characters is a value",
		isValueText(" api.github.com "),
		true,
	);
	is("a sentence is not a value", isValueText("derived from the API"), false);
	is("an empty cell is not a value", isValueText("  "), false);

	// The shape the census reads: one declared column of five rows, four of
	// them values and marked, the fifth a sentence left in its own block.
	const column = (cells) => ({
		...github,
		headings: ["Key", "Default"],
		columnCells: [
			cells.map(([key]) => ({ text: key, marked: false })),
			cells.map(([, text, marked]) => ({ text, marked })),
		],
	});
	const shipped = [
		["token", "required", true],
		["reserve_rate", "500", true],
		["timeout", "30s", true],
		["base_url", "api.github.com", true],
		["web_url", "derived from the API", false],
	];
	is(
		"a column that hoists every value it holds is counted, not just asked about",
		hoistCensus(column(shipped), "Default"),
		{ rows: 5, values: 4, hoisted: 4, unmarked: [], sentences: [] },
	);
	is(
		"a column that hoists every value it holds is right",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [column(shipped)],
		}),
		[],
	);
	is(
		"a value that stopped being marked is caught even though other rows still hoist",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [
				column([
					["token", "required", false],
					["reserve_rate", "500", true],
					["timeout", "30s", true],
					["base_url", "api.github.com", true],
					["web_url", "derived from the API", false],
				]),
			],
		}).length,
		1,
	);
	is(
		"a column reduced to one hoist of five is caught by the floor as well",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [
				column([
					["token", "required", false],
					["reserve_rate", "500", false],
					["timeout", "30s", false],
					["base_url", "api.github.com", true],
					["web_url", "derived from the API", false],
				]),
			],
		}).length,
		2,
	);
	is(
		"a table of two rows that hoists one of them is at the floor, not under it",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [
				column([
					["sinks.dedupe_horizon", "720h", true],
					["sinks.dedupe_file", "beside state_file", false],
				]),
			],
		}),
		[],
	);
	is(
		"a sentence hoisted into the corner is caught",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [
				column([
					["token", "required", true],
					["web_url", "derived from the API", true],
				]),
			],
		}).length,
		1,
	);
	is(
		"a column whose heading the frontmatter no longer names is not censused",
		hoistCensus(column(shipped), "Por omisión"),
		null,
	);
	is(
		"a table measured without a census is left to the other four checks",
		defaultColumnProblems({
			declared: [{ table: "Key", default: "Default" }],
			tables: [github],
		}),
		[],
	);

	is(
		"a break after a separator is one the name carries",
		breakIsAtBoundary({ after: ".", at: "d" }),
		true,
	);
	is(
		"a break after a space is one the name carries",
		breakIsAtBoundary({ after: " ", at: "-" }),
		true,
	);
	is(
		"a camel-case hump is one the name carries",
		breakIsAtBoundary({ after: "t", at: "H" }),
		true,
	);
	is(
		"the hump before a capitalised word is one too",
		breakIsAtBoundary({ after: "F", at: "A" }),
		true,
	);
	is(
		"a break between two ordinary letters is not",
		breakIsAtBoundary({ after: "z", at: "o" }),
		false,
	);
	is(
		"a key that broke at its own boundaries is right",
		keyBreakProblems([
			{
				text: "sinks.dedupe_horizon",
				breaks: [{ after: ".", at: "d" }],
			},
			{ text: "ProtectKernelTunables", breaks: [{ after: "t", at: "K" }] },
		]),
		[],
	);
	is(
		"a key broken mid-token is caught, and named with the character it followed",
		keyBreakProblems([
			{ text: "sinks.dedupe_horizon", breaks: [{ after: "o", at: "n" }] },
		]).length,
		1,
	);
	is(
		"a key measured as whole says nothing",
		keyBreakProblems([{ text: "token", breaks: [] }]),
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
	const {
		failures,
		tables,
		stacked,
		compact,
		indexes,
		defaultColumnsChecked,
		defaultCellsChecked,
		defaultRowsChecked,
		keysChecked,
		keysWrapped,
		routes,
	} = await walk(DIST);
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
				"boxes, or a default not beside its label, is the compact or " +
				"default form of the same file and of src/lib/rehype-tables.mjs; " +
				"a key or a hoisted value that took two line boxes is that form's " +
				"column cap, in the same file; a declared column that stopped " +
				"hoisting the cells it holds is isValue() in that plugin, or the " +
				"words in the table; a key broken away from its own boundaries " +
				"is KEY_BOUNDARY in that plugin, or overflow-wrap in the " +
				"stylesheet; a link that lands nowhere is the page's own.",
		);
		process.exit(1);
	}
	console.log(
		`[table-fit] ${tables} measurements over ${routes} pages at ` +
			`${WIDTHS.join(" and ")} px: every table fits its column ` +
			`(${stacked - compact} stacked, ${compact} compact indexes, ` +
			`${tables - stacked} still tables); all ${indexes} measurements of ` +
			"the index tables the sources declare rendered compact, every row " +
			`link lands, and the ${defaultColumnsChecked} measurements of the ` +
			"default columns the sources declare hoisted " +
			`${defaultCellsChecked} of their ${defaultRowsChecked} rows beside ` +
			"their key, which is every cell of them that is a value and no cell " +
			"that is not, each key and each hoisted value on one line. At a " +
			`${LARGE_ROOT}px root (200 % text) ${keysWrapped} of the ` +
			`${keysChecked} keys that are a whole name wrapped, every one of ` +
			"them at a boundary it carries.",
	);
} catch (error) {
	console.error(`[table-fit] ${error.message}`);
	process.exit(1);
}
