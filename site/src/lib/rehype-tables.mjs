/**
 * Prepares every prose table for the two shapes it has to render in: the wide
 * table, and the stacked rows a narrow column gets instead.
 *
 * Four things are added here, at build time, because markdown has nowhere to
 * write them and no author should have to:
 *
 *  - a SCROLL CONTAINER around the table. A table that overflows has to
 *    scroll, and the usual shortcut is `display: block; overflow-x: auto` on
 *    the table itself. That works and then quietly breaks the table:
 *    `display: block` drops the table layout, so the head and the body size
 *    their columns independently and a narrow screen shows a header of four
 *    compressed columns above rows twice as wide. The container is the fix:
 *    while it is wide the table keeps `display: table` and sizes its columns
 *    as one, and the wrapper is what scrolls. The wrapper is also the query
 *    container styles/tables.css decides the stacked form against, and it
 *    names the region, in the language of the page it is on, so the focus stop
 *    on it announces itself.
 *  - the COLUMN COUNT on that container, as `data-columns`. A two-column table
 *    and a five-column one do not need the same room, and `styles/tables.css`
 *    uses the number for both the width the wide form is held to and the width
 *    below which the table stops being one.
 *  - each body cell's COLUMN HEADING, as `data-label`. Stacked, a cell is no
 *    longer under its column, so it has to carry its heading with it.
 *  - the EXPLICIT ARIA ROLES every one of those elements already has
 *    implicitly. They are what survives the stacked form. A browser reads
 *    table semantics off the computed `display`, so the moment tables.css
 *    switches these elements to `block` the rows, the cells and the header
 *    association leave the accessibility tree and a phone is handed a sequence
 *    of anonymous blocks: still readable, because every cell prints its own
 *    heading, but no longer navigable as a table. Written out, the roles are
 *    exactly the implicit ones while the table is wide, which costs nothing,
 *    and they are the whole of the semantics once it is not.
 *
 *  - on a table its page names as an INDEX, `data-form="index"` on the
 *    container. See "Index tables" below.
 *
 *  - on a table its page names in `defaultColumns`, `data-form="default"` on
 *    the container and `data-role="default"` on the cell that holds the row's
 *    default, wherever that column sits, and only where that cell holds a
 *    value rather than a sentence. See "Default columns" below.
 *
 *  - `data-role="identity"` on a first cell that is the row's NAME, which is
 *    what styles/tables.css sizes as the block's title. See "The row's name"
 *    below.
 *
 * Why the stacked form exists at all, measured on the built site before it
 * did: all 98 tables of the corpus, in both languages, overflowed the column
 * they sit in at a 360 px and at a 400 px viewport, by 176 px to 417 px.
 * Taking the width floor off would not have fixed it. With the floor removed
 * and the content rendered as it is, the narrowest these tables can be AS
 * TABLES still ran to 745 px, because a heading cell and a code span are both
 * `white-space: nowrap`; the same tables with wrapping allowed need 83 px to
 * 204 px. The content fits a phone, the table shape does not.
 *
 * scripts/check-table-fit.mjs is the gate that keeps that true.
 */
import { localeOf, routeOf } from "./site.mjs";

/*
 * INDEX TABLES. Stacking is right for a reference table, whose cells are
 * clauses a reader reads, and wrong for an index, whose rows are short and
 * whose job is to let the reader find an item. Measured on the layouts page
 * before this existed, back when that page still carried an index: its
 * thirteen-row summary took 4,691 px stacked at a 360 px viewport, 337 px a
 * row, nearly six screens of labelled boxes before the first card, where the
 * same table is one glance on a desktop. An index gets a compact form instead
 * (styles/tables.css): one line per row naming the item, a smaller line under
 * it with the rest of the row.
 *
 * Which tables are indexes is the page's judgement, not this plugin's guess:
 * a page lists them in its frontmatter by the heading of their first column,
 *
 *     indexTables:
 *       - Family
 *
 * which reads the same in the source as in the rendered page, and survives a
 * table being added above it where a position would not. A name that matches
 * no table on the page is an error: a column renamed in the markdown would
 * otherwise put its table back into the stacked form without a word.
 *
 * An index row used to be linked here to the section of the page its first
 * cell named. The two pages that declare an index today, how/index.mdx and
 * sinks/index.mdx, cannot use it: the first names families that have no
 * section of their own, the second already writes its own links. The pass ran
 * over nothing, so it is gone rather than kept as a promise this file makes
 * and never keeps. A page that wants an index row to link writes the link.
 *
 * The markdown is not touched, so the twin, docs/ and llms-full.txt read the
 * table exactly as written.
 *
 * DEFAULT COLUMNS. A reference table (a key, a flag, a rule) stays a
 * reference: stacking is still right for it, its rows are not an index. But
 * its first cell used to spend a whole labelled block on the row's default,
 * one KEY/FLAG card followed by a whole DEFAULT card for one short value, and
 * the author read that on a phone and asked for the value beside the label
 * instead: same line, right-hand side, one block fewer per row.
 *
 * Which column that is is again the page's judgement, named in its
 * frontmatter as pairs of headings,
 *
 *     defaultColumns:
 *       - table: Key
 *         default: Default
 *
 * `table` is the heading that picks the table (every one whose first column
 * has that heading, so two tables sharing a heading, as configuration/'s two
 * `Key` tables do, both get it from one entry), `default` is the heading of
 * the column to hoist. A `table` naming no table on the page, or a `default`
 * naming no column of the table it did find, is an error for the same reason
 * a bad indexTables entry is: a column renamed in the markdown must not put
 * the default back in its own box without a word. A `default` naming the
 * FIRST column is an error too, since the row's name and the row's default
 * would be the same cell and the card would render with nothing on the left.
 * A table named in both `indexTables` and `defaultColumns` is also an error:
 * the compact form already runs every column but the first inline, so the two
 * treatments have nothing to agree on.
 *
 * WHICH CELLS, and why the page cannot decide this one. A hoisted cell is
 * read without its kicker: it is a value in the corner of the card, and the
 * position is what says "default". That works for a value and not for a
 * sentence. `web_url`'s default is "derived from the API": hoisted and
 * unlabelled it read as a fragment about the key, and `sinks.dedupe_file`'s,
 * a sentence with two code fragments in it, wrapped into four ragged
 * right-aligned lines with `<name>-written.bin` broken across two of them,
 * which read as broken markup. So the mechanism keys on what the CELL is, not
 * on what the page declared: `isValue()`, one unbroken run of characters with
 * no space in it, is marked and hoisted; anything else keeps the labelled
 * block it always had, in place, at the full width of the row. A declared
 * table whose every default is a sentence is an error rather than a silent
 * no-op. Backticks are not the test: this corpus writes `off`, `none`,
 * `required` and `api.github.com` without them and they are values all the
 * same, while an error message inside a code span is still a sentence.
 */

/*
 * THE ROW'S NAME. Stacked, a row is a card and its first cell is the card's
 * title, which styles/tables.css sizes apart from the values under it. That is
 * true of a first cell that NAMES the row, and false of two shapes this corpus
 * also has, so the name is marked here rather than guessed at in CSS:
 *
 *  - a first cell with no `code` in it is not marked. Every per-row identity
 *    on the tables this treatment was asked for is written as a code span
 *    (`token`, `-once`, `every.families`), and the cells that are not are
 *    prose rows like configuration/cadences.mdx's "the built-in table", which
 *    read as a heading floating over the row rather than as a title. 168 of
 *    the corpus's 718 stacked first cells hold no code span and stay at body
 *    size deliberately; /api/cost/'s thirty-four family names are the set
 *    worth backticking in the markdown one day, which is the page's decision
 *    and not this file's.
 *  - a first column that REPEATS names nothing. configuration/cadences.mdx's
 *    34-row `Group` table has `account` on eight consecutive rows, so eight
 *    cards would carry the same title while the cell that tells them apart,
 *    the family, sat below it in body type. A column with a duplicate in it
 *    is not an identifier, and no cell of that table is marked.
 *  - a page can also say so itself, in `plainTables`, by the heading of the
 *    first column, for a table whose first cell is a sentence that happens to
 *    contain a code span: configuration/index.mdx's `Remembers` and `Message`
 *    tables, whose first cells are "`last_head`, the commit each repository
 *    was on ..." and a whole validation message. `:has(code)` cannot tell
 *    those from `-card <path>` or from an install command, both of which are
 *    genuine names with a space in them, so the judgement is the page's, the
 *    way it already is for an index. A name matching no table on the page is
 *    an error, for the reason a bad indexTables entry is.
 */

/** What the region is called, per locale. */
const REGION_LABEL = { en: "Table", es: "Tabla" };

/** The role each of these elements carries implicitly, stated out loud. */
const ROLES = {
	table: "table",
	thead: "rowgroup",
	tbody: "rowgroup",
	tfoot: "rowgroup",
	tr: "row",
	td: "cell",
};

/** The text of a node and everything under it. */
function textOf(node) {
	if (!node) return "";
	if (node.type === "text") return node.value;
	if (!Array.isArray(node.children)) return "";
	return node.children.map(textOf).join("");
}

/** The element children of a node whose tag name is one of `names`. */
const childrenNamed = (node, names) =>
	(node?.children ?? []).filter(
		(child) => child.type === "element" && names.has(child.tagName),
	);

const ROW_GROUPS = new Set(["thead", "tbody", "tfoot"]);
const ROWS = new Set(["tr"]);
const CELLS = new Set(["td", "th"]);

/**
 * The heading of each column, read from the first row of the head. A table
 * with no head, which markdown cannot produce but raw HTML in a page could,
 * yields none and its cells go unlabelled rather than mislabelled. Such a
 * table keeps the wide form: tables.css stacks nothing it has no labels for.
 */
function headings(table) {
	const head = childrenNamed(table, ROW_GROUPS).find(
		(group) => group.tagName === "thead",
	);
	const row = childrenNamed(head, ROWS)[0];
	return childrenNamed(row, CELLS).map((cell) => textOf(cell).trim());
}

/**
 * Walks one table: the role on every part of it, and on every body cell the
 * heading of the column it sits under.
 *
 * A `th` is a header cell wherever it is, and which kind depends on where: in
 * the head it heads a column, anywhere else it heads its row. Markdown only
 * ever produces the first; the second is written out so that raw HTML in a
 * page is not left with a plain `cell` where it wrote a header.
 */
function prepare(table, columns) {
	table.properties.role = ROLES.table;
	for (const group of childrenNamed(table, ROW_GROUPS)) {
		group.properties.role = ROLES[group.tagName];
		const head = group.tagName === "thead";
		for (const row of childrenNamed(group, ROWS)) {
			row.properties.role = ROLES.tr;
			childrenNamed(row, CELLS).forEach((cell, index) => {
				cell.properties.role =
					cell.tagName === "th"
						? head
							? "columnheader"
							: "rowheader"
						: ROLES.td;
				if (head) return;
				const heading = columns[index];
				// An empty heading is a column that names itself, the left column
				// of a comparison matrix being this corpus's case. It gets no
				// label at all rather than an empty one, which would print as a
				// blank line above every cell of it.
				if (heading) cell.properties["data-label"] = heading;
			});
		}
	}
}

/**
 * The locale of the page being rendered, read off the file the tree came
 * from. A file this cannot place reads as English, which is the site's default
 * locale and is what every region was named before the locale was read at all.
 *
 * @param {{ path?: string } | undefined} file the vfile rehype passes through
 * @returns {"en" | "es"}
 */
function localeOfFile(file) {
	const match = /\/content\/docs\/(.*)$/.exec(file?.path ?? "");
	return match ? localeOf(routeOf(match[1])) : "en";
}

/**
 * The first-column headings of the tables a page declared as indexes, read
 * off the frontmatter Astro hands every plugin.
 *
 * @param {any} file the vfile rehype passes through
 * @returns {string[]}
 */
function indexTablesOf(file) {
	const declared = file?.data?.astro?.frontmatter?.indexTables;
	return Array.isArray(declared) ? declared.map(String) : [];
}

/**
 * The `{table, default}` pairs a page declared, read off the frontmatter
 * Astro hands every plugin.
 *
 * @param {any} file the vfile rehype passes through
 * @returns {{ table: string, default: string }[]}
 */
function defaultColumnsOf(file) {
	const declared = file?.data?.astro?.frontmatter?.defaultColumns;
	return Array.isArray(declared)
		? declared.map((entry) => ({
				table: String(entry.table),
				default: String(entry.default),
			}))
		: [];
}

/**
 * The first-column headings a page declared as plain: tables whose first cell
 * is not the row's name.
 *
 * @param {any} file the vfile rehype passes through
 * @returns {string[]}
 */
function plainTablesOf(file) {
	const declared = file?.data?.astro?.frontmatter?.plainTables;
	return Array.isArray(declared) ? declared.map(String) : [];
}

/**
 * Whether a cell holds a value rather than a sentence: one unbroken run of
 * characters with no space in it. `500`, `off`, `api.github.com` and `ninguno`
 * are values; "derived from the API" is not, and neither is a sentence that
 * happens to be written inside a code span.
 *
 * @param {any} cell
 * @returns {boolean}
 */
function isValue(cell) {
	const text = textOf(cell).trim();
	return text.length > 0 && !/\s/.test(text);
}

/** Whether a `code` element sits anywhere inside this node. */
function hasCode(node) {
	if (node.type === "element" && node.tagName === "code") return true;
	return (node.children ?? []).some((child) => hasCode(child));
}

/** The first cell of every body row of a table, in document order. */
function firstCells(table) {
	const cells = [];
	for (const group of childrenNamed(table, ROW_GROUPS)) {
		if (group.tagName === "thead") continue;
		for (const row of childrenNamed(group, ROWS)) {
			const cell = childrenNamed(row, CELLS)[0];
			if (cell) cells.push(cell);
		}
	}
	return cells;
}

/**
 * Marks every first cell that is the row's NAME with `data-role="identity"`,
 * which is what styles/tables.css sizes as the stacked card's title. See "The
 * row's name" above for the three cases this does not mark.
 *
 * @param {any} table
 */
function markIdentities(table) {
	const cells = firstCells(table);
	const names = cells.map((cell) => textOf(cell).trim());
	if (new Set(names).size !== names.length) return;
	for (const cell of cells) {
		if (hasCode(cell)) cell.properties["data-role"] = "identity";
	}
}

/**
 * Marks the cell at `columnIndex` of every body row whose default is a value
 * with `data-role="default"`, so styles/tables.css can grid-place it beside
 * the first cell's label whatever DOM position it started at. A cell holding a
 * sentence is left alone: unlabelled in the corner of a card it stops reading
 * as a default (see "Default columns" above).
 *
 * @param {any} table
 * @param {number} columnIndex
 * @returns {number} how many cells were marked
 */
function markDefaultColumn(table, columnIndex) {
	let hoisted = 0;
	for (const group of childrenNamed(table, ROW_GROUPS)) {
		if (group.tagName === "thead") continue;
		for (const row of childrenNamed(group, ROWS)) {
			const cell = childrenNamed(row, CELLS)[columnIndex];
			if (!cell || !isValue(cell)) continue;
			cell.properties["data-role"] = "default";
			hoisted += 1;
		}
	}
	return hoisted;
}

export default function rehypeTables() {
	/**
	 * @param {any} node
	 * @param {{
	 *   label: string,
	 *   indexes: Set<string>,
	 *   found: Set<string>,
	 *   defaults: Map<string, string>,
	 *   defaultsFound: Set<string>,
	 *   plain: Set<string>,
	 *   plainFound: Set<string>,
	 *   path: string,
	 * }} page
	 */
	const walk = (node, page) => {
		if (!node || !Array.isArray(node.children)) return;
		node.children = node.children.map((child) => {
			walk(child, page);
			if (child.type !== "element" || child.tagName !== "table") return child;
			const columns = headings(child);
			prepare(child, columns);
			const index = columns.length > 0 && page.indexes.has(columns[0]);
			if (index) page.found.add(columns[0]);
			const plain = columns.length > 0 && page.plain.has(columns[0]);
			if (plain) page.plainFound.add(columns[0]);
			else markIdentities(child);
			const defaultHeading =
				columns.length > 0 ? page.defaults.get(columns[0]) : undefined;
			let defaultColumn = false;
			if (defaultHeading !== undefined) {
				if (index) {
					throw new Error(
						`${page.path}: "${columns[0]}" is named in both indexTables and ` +
							"defaultColumns. A table cannot be both: the compact form " +
							"already runs every column but the first inline.",
					);
				}
				page.defaultsFound.add(columns[0]);
				const columnIndex = columns.indexOf(defaultHeading);
				if (columnIndex === -1) {
					throw new Error(
						`${page.path}: defaultColumns names "${defaultHeading}" as the ` +
							`default column of the "${columns[0]}" table, and that table ` +
							`has no column with that heading. Its columns are: ` +
							`${columns.map((name) => `"${name}"`).join(", ")}.`,
					);
				}
				if (columnIndex === 0) {
					throw new Error(
						`${page.path}: defaultColumns names "${defaultHeading}" as both ` +
							"the table to treat and the column to hoist. The first cell " +
							"is the row's name; hoisting it beside its own label would " +
							"leave the card with nothing on the left.",
					);
				}
				const hoisted = markDefaultColumn(child, columnIndex);
				if (hoisted === 0) {
					throw new Error(
						`${page.path}: defaultColumns names "${defaultHeading}" as the ` +
							`default column of the "${columns[0]}" table, and no row of ` +
							"it holds a value: every cell of that column is a sentence, " +
							"and a sentence keeps its own labelled block. Remove the " +
							"entry, or write those defaults as values.",
					);
				}
				defaultColumn = true;
			}
			return {
				type: "element",
				tagName: "div",
				properties: {
					className: ["table-scroll"],
					"data-columns": String(columns.length),
					...(index
						? { "data-form": "index" }
						: defaultColumn
							? { "data-form": "default" }
							: {}),
					// Focusable, because a region that scrolls has to be
					// reachable without a pointer. The role and the label are
					// what make that focus stop announce itself.
					tabindex: "0",
					role: "region",
					"aria-label": page.label,
				},
				children: [child],
			};
		});
	};
	// The tree is walked here rather than with unist-util-visit: that package
	// is present transitively, and depending on a transitive package is how a
	// build breaks on a machine whose resolution differs.
	return (tree, file) => {
		const defaultEntries = defaultColumnsOf(file);
		const page = {
			label: REGION_LABEL[localeOfFile(file)],
			indexes: new Set(indexTablesOf(file)),
			found: new Set(),
			defaults: new Map(
				defaultEntries.map((entry) => [entry.table, entry.default]),
			),
			defaultsFound: new Set(),
			plain: new Set(plainTablesOf(file)),
			plainFound: new Set(),
			path: file?.path ?? "a page",
		};
		walk(tree, page);
		const missing = [...page.indexes].filter((name) => !page.found.has(name));
		if (missing.length) {
			throw new Error(
				`${page.path}: indexTables names ${missing
					.map((name) => `"${name}"`)
					.join(", ")}, and no table on the page has that first column. ` +
					"Rename the entry to the table's first heading, or remove it.",
			);
		}
		const missingDefaults = [...page.defaults.keys()].filter(
			(name) => !page.defaultsFound.has(name),
		);
		if (missingDefaults.length) {
			throw new Error(
				`${page.path}: defaultColumns names ${missingDefaults
					.map((name) => `"${name}"`)
					.join(", ")}, and no table on the page has that first column. ` +
					"Rename the entry to the table's first heading, or remove it.",
			);
		}
		const missingPlain = [...page.plain].filter(
			(name) => !page.plainFound.has(name),
		);
		if (missingPlain.length) {
			throw new Error(
				`${page.path}: plainTables names ${missingPlain
					.map((name) => `"${name}"`)
					.join(", ")}, and no table on the page has that first column. ` +
					"Rename the entry to the table's first heading, or remove it.",
			);
		}
	};
}
