/**
 * Prepares every prose table for the two shapes it has to render in: the wide
 * table, and the stacked rows a narrow column gets instead.
 *
 * Three things are added here, at build time, because markdown has nowhere to
 * write them and no author should have to:
 *
 *  - a SCROLL CONTAINER around the table. A table that overflows has to
 *    scroll, and the usual shortcut is `display: block; overflow-x: auto` on
 *    the table itself. That works and then quietly breaks the table:
 *    `display: block` drops the table layout, so the head and the body size
 *    their columns independently and a narrow screen shows a header of four
 *    compressed columns above rows twice as wide. The container is the fix:
 *    the table keeps `display: table` and sizes its columns as one, the
 *    wrapper scrolls.
 *  - the COLUMN COUNT on that container, as `data-columns`. A two-column table
 *    and a five-column one do not need the same room, and `styles/tables.css`
 *    uses the number for both the width the wide form is held to and the width
 *    below which the table stops being one.
 *  - each body cell's COLUMN HEADING, as `data-label`. Stacked, a cell is no
 *    longer under its column, so it has to carry its heading with it.
 *
 * Why the stacked form exists at all, measured on the built site before it
 * did: all 98 tables of the corpus, in both languages, overflowed the column
 * they sit in at a 360 px and at a 400 px viewport, by 176 px to 417 px.
 * Taking the width floor off would not have fixed it. With the floor removed
 * and the content rendered as it is, the narrowest these tables can be AS
 * TABLES still ran to 745 px, because a heading cell and a code span are both
 * `white-space: nowrap`; the same tables with wrapping allowed need 83 px to
 * 204 px. The content fits a phone, the table shape does not.
 */

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
 * yields none, and its cells go unlabelled rather than mislabelled.
 */
function headings(table) {
	const head = childrenNamed(table, ROW_GROUPS).find(
		(group) => group.tagName === "thead",
	);
	const row = childrenNamed(head, ROWS)[0];
	return childrenNamed(row, CELLS).map((cell) => textOf(cell).trim());
}

/** Stamps every body cell with the heading of the column it sits under. */
function label(table, columns) {
	for (const group of childrenNamed(table, ROW_GROUPS)) {
		if (group.tagName === "thead") continue;
		for (const row of childrenNamed(group, ROWS)) {
			childrenNamed(row, CELLS).forEach((cell, index) => {
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

export default function rehypeTables() {
	/** @param {any} node */
	const walk = (node) => {
		if (!node || !Array.isArray(node.children)) return;
		node.children = node.children.map((child) => {
			walk(child);
			if (child.type !== "element" || child.tagName !== "table") return child;
			const columns = headings(child);
			label(child, columns);
			return {
				type: "element",
				tagName: "div",
				properties: {
					className: ["table-scroll"],
					"data-columns": String(columns.length),
					// Focusable, because a region that scrolls has to be
					// reachable without a pointer. The role and the label are
					// what make that focus stop announce itself.
					tabindex: "0",
					role: "region",
					"aria-label": "Table",
				},
				children: [child],
			};
		});
	};
	// The tree is walked here rather than with unist-util-visit: that package
	// is present transitively, and depending on a transitive package is how a
	// build breaks on a machine whose resolution differs.
	return (tree) => walk(tree);
}
