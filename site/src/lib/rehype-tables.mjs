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
 *    container, and a link from each row's first cell to the section of the
 *    page that cell names, when there is one. See "Index tables" below.
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
 * before this existed: its thirteen-row summary took 4,691 px stacked at a
 * 360 px viewport, 337 px a row, nearly six screens of labelled boxes before
 * the first card, where the same table is one glance on a desktop. An index
 * gets a compact form instead (styles/tables.css): one line per row naming the
 * item, a smaller line under it with the rest of the row.
 *
 * Which tables are indexes is the page's judgement, not this plugin's guess:
 * a page lists them in its frontmatter by the heading of their first column,
 *
 *     indexTables:
 *       - Layout
 *
 * which reads the same in the source as in the rendered page, and survives a
 * table being added above it where a position would not. A name that matches
 * no table on the page is an error: a column renamed in the markdown would
 * otherwise put its table back into the stacked form without a word.
 *
 * The markdown is not touched, so the twin, docs/ and llms-full.txt read the
 * table exactly as written, and so does internal/render/documented_test.go,
 * which parses the layouts table's rows as markdown.
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
 * The anchor each section heading of the page carries, by its text.
 *
 * The ids are read, never derived here. Astro's own `rehypeHeadingIds` runs
 * after every configured plugin, so astro.config.mjs also places it directly
 * before this one: the ids exist by the time this reads them, they are made by
 * the one slugger that makes them for the page (duplicates suffixed in
 * document order), and the later built-in pass keeps an id it finds. A
 * second derivation here could disagree with that slugger on two headings
 * that slug alike and send a row to the wrong section with nothing to notice.
 * A heading without an id is not linked, and neither is a heading text that
 * appears twice, which has two anchors and no single answer.
 *
 * @param {any} tree
 * @returns {Map<string, string | null>} heading text to id
 */
function sectionAnchors(tree) {
	const anchors = new Map();
	const visit = (node) => {
		if (node?.type === "element" && /^h[1-6]$/.test(node.tagName)) {
			const id = node.properties?.id;
			if (typeof id !== "string" || id === "") return;
			const text = textOf(node).trim();
			anchors.set(text, anchors.has(text) ? null : id);
			return;
		}
		for (const child of node?.children ?? []) visit(child);
	};
	visit(tree);
	return anchors;
}

/**
 * Links each row's first cell to the section named after it, when the page
 * has exactly one. The cell's own content, a code span in this corpus, becomes
 * the link's content, so the row reads as it did and the link's accessible
 * name is the item's name.
 *
 * @param {any} table
 * @param {Map<string, string>} anchors
 */
function linkRows(table, anchors) {
	for (const group of childrenNamed(table, ROW_GROUPS)) {
		if (group.tagName === "thead") continue;
		for (const row of childrenNamed(group, ROWS)) {
			const first = childrenNamed(row, CELLS)[0];
			if (!first) continue;
			const alreadyLinked = (node) =>
				node.type === "element" &&
				(node.tagName === "a" || (node.children ?? []).some(alreadyLinked));
			if (first.children.some(alreadyLinked)) continue;
			const id = anchors.get(textOf(first).trim());
			if (!id) continue;
			first.children = [
				{
					type: "element",
					tagName: "a",
					properties: { href: `#${id}` },
					children: first.children,
				},
			];
		}
	}
}

export default function rehypeTables() {
	/**
	 * @param {any} node
	 * @param {{ label: string, indexes: Set<string>, found: Set<string>, anchors: Map<string, string> }} page
	 */
	const walk = (node, page) => {
		if (!node || !Array.isArray(node.children)) return;
		node.children = node.children.map((child) => {
			walk(child, page);
			if (child.type !== "element" || child.tagName !== "table") return child;
			const columns = headings(child);
			prepare(child, columns);
			const index = columns.length > 0 && page.indexes.has(columns[0]);
			if (index) {
				page.found.add(columns[0]);
				linkRows(child, page.anchors);
			}
			return {
				type: "element",
				tagName: "div",
				properties: {
					className: ["table-scroll"],
					"data-columns": String(columns.length),
					...(index ? { "data-form": "index" } : {}),
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
		const indexes = new Set(indexTablesOf(file));
		const page = {
			label: REGION_LABEL[localeOfFile(file)],
			indexes,
			found: new Set(),
			anchors: indexes.size ? sectionAnchors(tree) : new Map(),
		};
		walk(tree, page);
		const missing = [...indexes].filter((name) => !page.found.has(name));
		if (missing.length) {
			throw new Error(
				`${file?.path ?? "a page"}: indexTables names ${missing
					.map((name) => `"${name}"`)
					.join(", ")}, and no table on the page has that first column. ` +
					"Rename the entry to the table's first heading, or remove it.",
			);
		}
	};
}
