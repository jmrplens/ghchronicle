// Heading structure and accessible names, over the built site.
//
// html-validate and htmlhint check that the markup is well formed; neither
// checks that a page has one outline, that a repeated landmark can be told
// apart from its twin, or that every control can be named out loud. pa11y
// checks all of that, but only on the handful of URLs it is given, because it
// drives a real browser and cannot afford 73 of them on every push. This runs
// over every built page, in about a second, and catches the case pa11y's
// sample would miss: one page template out of ten losing its labels.
//
// Deliberately scoped to <main> for the outline: the site title, the sidebar
// and the mobile navigation all live outside it and are not part of the
// document's content.
//
// Usage: node scripts/check-headings-labels.mjs [dist-directory]
import { readFileSync, readdirSync } from "node:fs";
import { join, resolve } from "node:path";
import process from "node:process";

const DIST = resolve(process.argv[2] ?? "dist");

// Elements that are landmarks wherever they appear. <section> and <form> are
// landmarks only when named, which is the case this check cannot fail: an
// unnamed one is not a landmark at all, so it has no twin to be confused with.
const LANDMARKS = ["main", "nav", "header", "footer", "aside"];

// The one landmark on the site that repeats without a name, and is not this
// site's to name: Starlight's PageFrame wraps the table of contents in
// `<aside class="right-sidebar-container">`. The <nav> inside it is named, so
// the content is announced; naming the container would mean replacing the
// component that emits it. Listed as one exception rather than switching the
// rule off, so a second unnamed landmark still fails.
const EXEMPT_LANDMARKS = [/\bclass="[^"]*\bright-sidebar-container\b/];

/** Every built page. */
function* htmlFiles(dir) {
	for (const entry of readdirSync(dir, { withFileTypes: true })) {
		const full = join(dir, entry.name);
		if (entry.isDirectory()) yield* htmlFiles(full);
		else if (entry.isFile() && entry.name.endsWith(".html")) yield full;
	}
}

const stripTags = (html) =>
	html
		.replaceAll(/<(script|style)\b[\s\S]*?<\/\1>/g, " ")
		.replaceAll(/<[^>]+>/g, " ")
		.replaceAll(/&[a-z#0-9]+;/gi, " ")
		.trim();

/** The value of one attribute of a tag, or undefined. */
const attribute = (tag, name) =>
	new RegExp(`\\s${name}\\s*=\\s*"([^"]*)"`, "i").exec(tag)?.[1] ??
	new RegExp(`\\s${name}\\s*=\\s*'([^']*)'`, "i").exec(tag)?.[1];

/** Whether a tag carries an attribute at all, valued or not. */
const hasAttribute = (tag, name) =>
	new RegExp(`\\s${name}(\\s|=|>|/)`, "i").test(tag);

/**
 * A static approximation of the accessible name computation: enough to tell an
 * element that can be named from one that cannot be named at all, which is the
 * failure worth a gate. It deliberately does not rank the sources the way a
 * browser does, because the answer it needs is a boolean.
 */
const named = (tag, inner, page) =>
	Boolean(
		attribute(tag, "aria-label")?.trim() ||
		attribute(tag, "aria-labelledby")?.trim() ||
		attribute(tag, "title")?.trim() ||
		stripTags(inner ?? "") ||
		// An image with alt text names the link or button that wraps it.
		/<img[^>]*\salt\s*=\s*"[^"]+"/i.test(inner ?? "") ||
		// An inline <svg> names it through its own <title>.
		/<title[^>]*>[^<]+<\/title>/i.test(inner ?? "") ||
		// ...or through a title referenced by aria-labelledby, which is how the
		// icons in this site's chrome are named.
		[...(inner ?? "").matchAll(/aria-labelledby\s*=\s*"([^"]+)"/g)].some(
			(match) =>
				match[1]
					.split(/\s+/)
					.some((id) => new RegExp(`\\sid\\s*=\\s*"${id}"`).test(page)),
		),
	);

/** Every <tag ...>inner</tag> pair, non-nesting, which is all these need. */
function* pairs(html, tag) {
	const re = new RegExp(`<${tag}\\b([^>]*)>([\\s\\S]*?)</${tag}>`, "gi");
	for (const match of html.matchAll(re)) {
		yield { tag: `<${tag}${match[1]}>`, attrs: match[1], inner: match[2] };
	}
}

const problems = [];
let pages = 0;

for (const file of htmlFiles(DIST)) {
	const html = readFileSync(file, "utf8");
	const page = file.slice(DIST.length).replace(/\/index\.html$/, "/");
	const report = (message) => problems.push(`${page}: ${message}`);
	pages += 1;

	if (!/<html[^>]*\slang\s*=\s*"[a-z-]+"/i.test(html)) {
		report("<html> has no lang attribute");
	}

	// A landmark that appears once is unambiguous. A second one of the same kind
	// is not, and a screen reader lists them side by side, so each needs a name.
	for (const landmark of LANDMARKS) {
		const found = [...pairs(html, landmark)];
		if (found.length < 2) continue;
		for (const element of found) {
			if (EXEMPT_LANDMARKS.some((pattern) => pattern.test(element.tag)))
				continue;
			if (!named(element.tag, "", html)) {
				report(
					`one of ${found.length} <${landmark}> elements has no accessible name: ${element.tag.slice(0, 90)}`,
				);
			}
		}
	}

	for (const match of html.matchAll(/<img\b([^>]*)>/gi)) {
		// An empty alt is the documented way to say "decorative", so it passes.
		// A missing one leaves a screen reader reading out the file name.
		if (!hasAttribute(match[1], "alt")) {
			report(`<img> without an alt attribute: ${match[0].slice(0, 90)}`);
		}
	}

	for (const tag of ["a", "button"]) {
		for (const element of pairs(html, tag)) {
			// An anchor with no href is not a link and needs no name.
			if (tag === "a" && !hasAttribute(element.attrs, "href")) continue;
			if (!named(element.tag, element.inner, html)) {
				report(
					`<${tag}> with no discernible text: ${(element.tag + element.inner).slice(0, 90)}`,
				);
			}
		}
	}

	for (const match of html.matchAll(/<(input|select|textarea)\b([^>]*)>/gi)) {
		const [whole, element, attrs] = match;
		const type = (attribute(attrs, "type") ?? "").toLowerCase();
		// A hidden input is not a control, and a button-like input is named by its
		// own value rather than by a label.
		if (["hidden", "submit", "reset", "button", "image"].includes(type)) {
			continue;
		}
		const id = attribute(attrs, "id");
		const labelled =
			attribute(attrs, "aria-label")?.trim() ||
			attribute(attrs, "aria-labelledby")?.trim() ||
			attribute(attrs, "title")?.trim() ||
			(id && new RegExp(`<label[^>]*\\sfor\\s*=\\s*"${id}"`, "i").test(html)) ||
			// A control wrapped in its own <label> is labelled by the text beside it.
			new RegExp(
				`<label\\b[^>]*>(?:(?!</label>)[\\s\\S])*${element}\\b[^>]*${
					id ? `id\\s*=\\s*"${id}"` : ""
				}`,
				"i",
			).test(html);
		if (!labelled) {
			report(`<${element}> with no label: ${whole.slice(0, 90)}`);
		}
	}

	const main = /<main\b[^>]*>([\s\S]*?)<\/main>/i.exec(html);
	if (!main) {
		report("no <main> landmark");
		continue;
	}
	const levels = [...main[1].matchAll(/<h([1-6])\b[^>]*>/gi)].map((match) =>
		Number(match[1]),
	);
	const h1s = levels.filter((level) => level === 1).length;
	if (h1s !== 1) report(`<main> has ${h1s} <h1>, expected exactly 1`);
	if (levels.length > 0 && levels[0] !== 1) {
		report(`<main> starts at h${levels[0]}, expected h1`);
	}
	for (let i = 1; i < levels.length; i += 1) {
		if (levels[i] > levels[i - 1] + 1) {
			report(`heading level skips h${levels[i - 1]} to h${levels[i]}`);
		}
	}
}

if (pages === 0) {
	console.error(
		`[headings-labels] no HTML under ${DIST}. Run pnpm build first.`,
	);
	process.exit(1);
}

if (problems.length > 0) {
	console.error(
		`[headings-labels] ${problems.length} problem(s) across ${pages} pages:`,
	);
	for (const problem of problems) console.error(`  ${problem}`);
	process.exit(1);
}

console.log(
	`[headings-labels] ${pages} pages: one outline each, landmarks and controls named.`,
);
