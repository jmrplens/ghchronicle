// The markdown twin of a page: the same words, without the chrome.
//
// A rendered page is mostly navigation. The sidebar, the table of contents,
// the search index and the syntax highlighting are the bulk of the bytes, and
// none of them are the page. An agent that wants one page has had two options:
// parse the HTML around all of that, or download the whole corpus. The twin is
// the third: the page's own markdown, at the page's own path with `index.md`
// appended, announced from the page with `<link rel="alternate">`.
//
// MDX components are reduced, not dropped. An <Aside> becomes a blockquote
// with its title, a <TabItem> keeps its label, a <LinkCard> becomes a link
// carrying its description. A component this file does not know throws, so
// adding one to the content fails the build here instead of quietly deleting a
// section from the twin that the HTML still shows.
import figures from "../data/figures.json" with { type: "json" };
import stats from "../data/stats.json" with { type: "json" };

import { localeOf, pageUrl } from "./site.mjs";

// Starlight's own default titles for an untitled aside, in the two locales the
// site publishes. An aside whose title is left out renders one of these in the
// HTML, so the twin says the same thing rather than losing the distinction
// between a note and a warning.
const ASIDE_TITLES = {
	en: { note: "Note", tip: "Tip", caution: "Caution", danger: "Danger" },
	es: {
		note: "Nota",
		tip: "Consejo",
		caution: "Precaución",
		danger: "Peligro",
	},
};

// Matches a component tag, opening, closing or self-closing, across newlines:
// several <LinkCard> tags in the content spread their attributes over four
// lines, and `.` would stop at the first of them.
const TAG =
	/<(\/?)([A-Z][A-Za-z0-9]*)((?:"[^"]*"|'[^']*'|\{[^}]*\}|[^>"'{])*?)(\/?)>/;

/**
 * The double-quoted attributes of a tag. Every component in the content writes
 * its props that way; a prop written as an expression is not readable as text
 * anyway and is left out rather than printed as source.
 *
 * @param {string} raw the text between the tag name and the closing bracket
 * @returns {Record<string, string>} attribute name to value
 */
function parseAttributes(raw) {
	return Object.fromEntries(
		[...raw.matchAll(/([A-Za-z][\w-]*)\s*=\s*"([^"]*)"/g)].map((match) => [
			match[1],
			match[2],
		]),
	);
}

/** @param {string} text @returns {string} the text as a markdown blockquote. */
const blockquote = (text) =>
	text
		.trim()
		.split("\n")
		.map((line) => (line.trim() ? `> ${line}` : ">"))
		.join("\n");

/**
 * Removes the indentation a component's children carry because of the tag
 * around them, keeping their indentation relative to each other. Left alone,
 * the children of a <TabItem> inside a <Steps> list arrive seven spaces in,
 * which markdown reads as a code block rather than as the prose it is.
 *
 * @param {string} text the children of a component
 * @returns {string} the same lines, shifted to the left margin
 */
function dedent(text) {
	const lines = text.split("\n");
	const widths = lines
		.filter((line) => line.trim())
		.map((line) => (/^[ \t]*/.exec(line) ?? [""])[0].length);
	const shift = widths.length > 0 ? Math.min(...widths) : 0;
	return lines.map((line) => line.slice(shift)).join("\n");
}

/**
 * Puts a block back where its opening tag sat, so a component nested in a list
 * item stays inside that list item.
 *
 * @param {string} text a rendered block
 * @param {string} indent the indentation of the line the tag opened on
 * @returns {string} the block, indented
 */
const indentBy = (text, indent) =>
	indent
		? text
				.split("\n")
				.map((line) => (line.trim() ? indent + line : line))
				.join("\n")
		: text;

// The data modules a page may read a number out of, by the name the page
// imports them under. A count the repository already holds is written into the
// page as an expression rather than typed, and MDX evaluates it; this file
// does not, so without the substitution below the reduction publishes the
// source of the expression. It did: `{stats.panels}` reached docs/, every
// markdown twin and both llms-full.txt, where the number that should have
// said one hundred and fifty two said nothing at all.
const DATA = { stats };

// An expression of the shape a page writes: a name this file may know, and one
// key of it.
const EXPRESSION = /\{([A-Za-z_$][\w$]*)\.([\w$]+)\}/g;

/**
 * A copy of the text with its fenced blocks blanked out, so a guard that reads
 * prose does not read a code sample. The fences themselves stay, so line
 * numbers and offsets are unchanged.
 *
 * @param {string} text
 * @returns {string} the prose of the text
 */
const proseOnly = (text) =>
	text.replace(/^(```+|~~~+).*\n[\s\S]*?^\1[^\n]*$/gm, (block) =>
		block.replaceAll(/[^\n]/g, " "),
	);

/**
 * Resolves the data expressions a page writes, and refuses to publish one it
 * cannot.
 *
 * A key DATA does not hold is a typo that the HTML would render as `undefined`
 * and the reduction would publish verbatim, so it throws by name. A name DATA
 * does not hold is a data module nobody taught this file about, which is the
 * failure that produced the leak in the first place; it throws too, and only
 * outside fenced code, because a code sample is allowed to contain braces.
 *
 * @param {string} source MDX body, imports already dropped
 * @param {{ file: string }} context the page it came from, named in any failure
 * @returns {string} the body with every expression replaced by its value
 */
function resolveData(source, context) {
	const resolved = source.replaceAll(EXPRESSION, (whole, name, key) => {
		if (!(name in DATA)) return whole;
		const value = /** @type {Record<string, unknown>} */ (DATA[name])[key];
		if (value === undefined) {
			throw new Error(
				`${context.file}: ${whole} has no value: ${name} holds ` +
					`${Object.keys(DATA[name]).join(", ")}`,
			);
		}
		return String(value);
	});
	const left = EXPRESSION.exec(proseOnly(resolved));
	EXPRESSION.lastIndex = 0;
	if (left) {
		throw new Error(
			`${context.file}: ${left[0]} is an expression this file cannot ` +
				"resolve, so the markdown twin and docs/ would publish it as " +
				"written. Add its module to DATA in site/src/lib/page-markdown.mjs.",
		);
	}
	return resolved;
}

/** @param {string} text @returns {string} the text with its blank lines collapsed. */
const tidy = (text) => text.replace(/[ \t]+$/gm, "").replace(/\n{3,}/g, "\n\n");

/**
 * Renders one component that wraps content.
 *
 * @param {string} name component name
 * @param {Record<string, string>} attributes its double-quoted props
 * @param {string} children the already reduced children
 * @param {{ file: string, locale: "en" | "es" }} context page being rendered
 * @returns {string} markdown
 */
function renderWrapper(name, attributes, children, context) {
	switch (name) {
		case "Aside": {
			const type = attributes.type ?? "note";
			const fallback =
				ASIDE_TITLES[context.locale][
					/** @type {keyof typeof ASIDE_TITLES["en"]} */ (type)
				] ?? type;
			const title = attributes.title ?? fallback;
			return `\n\n${blockquote(`**${title}**\n\n${children.trim()}`)}\n\n`;
		}
		// A tab strip is a set of alternatives, and that is what it stays: one
		// list item per tab, its label leading it. The label alone on a line was
		// a heading pretending not to be one, which is exactly what
		// markdownlint's MD036 says of it where the reduction is linted
		// (docs/, via scripts/gen-docs.mjs). A list also keeps each tab's body
		// attached to its own label rather than to whatever follows.
		case "TabItem":
			return `\n\n- **${attributes.label ?? ""}**\n\n${indentBy(children.trim(), "  ")}\n\n`;
		// Pure layout around content that is already markdown: an ordered list for
		// <Steps>, a list of files for <FileTree>, a grid of cards. Unwrapping them
		// leaves exactly what the page shows.
		case "Tabs":
		case "Steps":
		case "CardGrid":
		case "FileTree":
			return `\n\n${children.trim()}\n\n`;
		default:
			throw new Error(
				`${context.file}: no markdown reduction for <${name}>. ` +
					"Add one in src/lib/page-markdown.mjs so the twin keeps saying what the page says.",
			);
	}
}

/**
 * Renders one self-closing component.
 *
 * @param {string} name component name
 * @param {Record<string, string>} attributes its double-quoted props
 * @param {{ file: string, locale: "en" | "es" }} context page being rendered
 * @returns {string} markdown
 */
function renderSelfClosing(name, attributes, context) {
	// A figure is a drawing, and a drawing reduces to the words it draws. Both
	// the description and the table below are generated beside the SVG by
	// scripts/gen-figures.mjs from the same values, so the twin cannot come to
	// say something the picture does not, and a reader of docs/ or of
	// llms-full.txt gets the figure's content rather than a hole where a
	// component was.
	if (name === "Figure") {
		const figure = figures[attributes.name];
		if (!figure) {
			throw new Error(
				`${context.file}: <Figure name="${attributes.name}" /> is not a figure. ` +
					`src/data/figures.json holds ${Object.keys(figures).join(", ")}; ` +
					"add it to FIGURES in site/scripts/gen-figures.mjs and run pnpm run figures.",
			);
		}
		return `\n\n${figure[context.locale].markdown}\n\n`;
	}
	if (name === "LinkCard") {
		const title = attributes.title ?? attributes.href ?? "";
		const link = attributes.href ? `[${title}](${attributes.href})` : title;
		return `\n- ${link}${attributes.description ? `: ${attributes.description}` : ""}\n`;
	}
	throw new Error(
		`${context.file}: no markdown reduction for <${name} />. ` +
			"Add one in src/lib/page-markdown.mjs so the twin keeps saying what the page says.",
	);
}

/**
 * Reduces MDX to markdown, resolving components as it goes.
 *
 * @param {string} source MDX body, without frontmatter
 * @param {{ file: string, locale: "en" | "es" }} context page being rendered
 * @returns {string} markdown
 */
function reduce(source, context) {
	let out = "";
	let rest = source;
	for (;;) {
		const match = TAG.exec(rest);
		if (!match) return out + rest;
		const [whole, closing, name, raw, selfClosing] = match;
		out += rest.slice(0, match.index);
		rest = rest.slice(match.index + whole.length);
		if (closing) {
			throw new Error(`${context.file}: </${name}> without an opening tag`);
		}
		const attributes = parseAttributes(raw);
		if (selfClosing) {
			out += renderSelfClosing(name, attributes, context);
			continue;
		}
		// Scan to the matching close, counting nested tags of the same name so a
		// component inside another of its kind closes the inner one first.
		const scanner = new RegExp(`<${name}(?=[\\s/>])|</${name}>`, "g");
		let depth = 1;
		let end = -1;
		let after = -1;
		for (let inner = scanner.exec(rest); inner; inner = scanner.exec(rest)) {
			depth += inner[0].startsWith("</") ? -1 : 1;
			if (depth === 0) {
				end = inner.index;
				after = scanner.lastIndex;
				break;
			}
		}
		if (end === -1) {
			throw new Error(`${context.file}: <${name}> is never closed`);
		}
		const children = rest.slice(0, end);
		rest = rest.slice(after);
		const indent = (/(?:^|\n)([ \t]*)$/.exec(out) ?? ["", ""])[1];
		out += indentBy(
			renderWrapper(
				name,
				attributes,
				dedent(reduce(children, context)),
				context,
			),
			indent,
		);
	}
}

/**
 * The body of a page as markdown: imports dropped, components resolved, blank
 * lines tidied.
 *
 * The twin this file publishes and the plain-Markdown copy under docs/ are the
 * same reduction of the same source, so the two cannot come to say different
 * things; only what wraps the body differs. See site/scripts/gen-docs.mjs.
 *
 * @param {object} page
 * @param {string} page.body MDX body, without frontmatter
 * @param {string} page.file source path, named in any failure
 * @param {"en" | "es"} page.locale the locale the page is served in
 * @returns {string} markdown
 */
export function reduceBody({ body, file, locale }) {
	const markdown = tidy(
		resolveData(reduce(stripImports(body), { file, locale }), { file }),
	).trim();
	if (!markdown) throw new Error(`${file}: nothing left after reduction`);
	return markdown;
}

/**
 * The markdown twin of one documentation page.
 *
 * @param {object} page
 * @param {string} page.route the page route, "" for the English home
 * @param {string} page.title frontmatter title
 * @param {string} page.description frontmatter description
 * @param {string} page.body MDX body, or already reduced markdown
 * @param {string} page.file source path, named in any failure
 * @returns {string} the twin document
 */
export function renderTwin({ route, title, description, body, file }) {
	const markdown = reduceBody({ body, file, locale: localeOf(route) });
	return `${[
		`# ${title}`,
		"",
		description,
		"",
		`Source: ${pageUrl(route)}`,
		"",
		markdown,
	].join("\n")}\n`;
}

/**
 * Drops the MDX import block. It names the components this file has just
 * resolved, so keeping it would leave a reader instructions for machinery that
 * is no longer there.
 *
 * @param {string} source MDX body
 * @returns {string} the body without its imports
 */
function stripImports(source) {
	return source
		.replace(/^import\s+\{[^}]*\}\s+from\s+["'][^"']+["'];?\s*$/gms, "")
		.replace(/^import\s+.+?\s+from\s+["'][^"']+["'];?\s*$/gm, "");
}
