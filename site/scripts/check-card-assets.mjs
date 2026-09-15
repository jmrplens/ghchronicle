#!/usr/bin/env node
/**
 * Gates that every card picture in `src/assets/` is shown on some page.
 *
 * The documentation is where a reader sees what the cards look like, and the
 * gallery test writes more pictures than a page happens to ask for. Nothing
 * failed when one was left out: `card-wide-banner-loop.svg` and
 * `card-sparkline-hero-loop.svg`, with their dark palettes, sat in the assets
 * for a release with no page showing them, while the build, the twins and
 * every other gate passed.
 *
 * A page shows a card through `<Card>`, and the component decides the files
 * from its props (`src/components/Card.astro`):
 *
 *   <Card name="x">       card-x.svg and card-x_dark.svg
 *   <Card name="x" loop>  those two, and card-x-loop.svg and card-x-loop_dark.svg
 *
 * Every `card-*.svg` file that no page's `<Card>` accounts for fails, named,
 * with the invocation that would show it. There is no allow-list: a picture
 * nobody shows is either missing from a page or should not be generated.
 *
 * Each locale is gated on its own. A reader of the Spanish pages never sees an
 * English one, and `check-i18n-parity.mjs` compares a card's name across the
 * twins but not its `loop`, so a card dropped or a toggle left out on one side
 * would otherwise pass here on the strength of the other.
 *
 * The pages are parsed with the MDX parser the site renders with, as
 * `check-i18n-parity.mjs` does, so a `<Card>` quoted in a code block or an
 * inline code span is text, not a card, and cannot count as one. A `name`
 * written as an expression is refused, because the files it names cannot be
 * known without evaluating the page.
 *
 * The fixtures at the bottom run on every invocation.
 *
 * Usage:
 *   node scripts/check-card-assets.mjs              # exit 0 when every card is shown
 *   node scripts/check-card-assets.mjs --self-test  # fixtures only, no corpus
 */
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import { fromMarkdown } from "mdast-util-from-markdown";
import { mdxFromMarkdown } from "mdast-util-mdx";
import { mdxjs } from "micromark-extension-mdxjs";

const DOCS_DIR = fileURLToPath(new URL("../src/content/docs", import.meta.url));
const ASSETS_DIR = fileURLToPath(new URL("../src/assets", import.meta.url));
const CARD_FILE = /^card-.+\.svg$/;
// The mirror locales directly under DOCS_DIR, as in check-i18n-parity.mjs; a
// page under none of them is English.
const LOCALES = { en: "English", es: "Spanish" };

/**
 * The locale a page belongs to, by its first directory.
 *
 * @param {string} page a path relative to DOCS_DIR
 * @returns {string} a key of LOCALES
 */
const localeOf = (page) => {
	const first = page.split("/")[0];
	return first !== "en" && first in LOCALES ? first : "en";
};

/**
 * Every content page under `dir`, as paths relative to it.
 *
 * @param {string} dir
 * @param {string} [prefix]
 * @returns {string[]}
 */
function listPages(dir, prefix = "") {
	return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
		const relative = prefix ? `${prefix}/${entry.name}` : entry.name;
		if (entry.isDirectory())
			return listPages(path.join(dir, entry.name), relative);
		return /\.mdx?$/.test(entry.name) ? [relative] : [];
	});
}

/**
 * The `<Card>` invocations of one page.
 *
 * @param {string} source the page, frontmatter included
 * @param {string} file the page's path, named in any failure
 * @returns {{ name: string, loop: boolean }[]}
 */
function cardsIn(source, file) {
	// Frontmatter is YAML, not markdown: blank it so the parser never reads it,
	// keeping the line count so a position still points at the page.
	const body = source.replace(/^---\n[\s\S]*?\n---(?=\n|$)/, (block) =>
		block.replaceAll(/[^\n]/g, ""),
	);
	const tree = fromMarkdown(body, {
		extensions: file.endsWith(".mdx") ? [mdxjs()] : [],
		mdastExtensions: file.endsWith(".mdx") ? [mdxFromMarkdown()] : [],
	});
	const cards = [];
	const visit = (node) => {
		if (
			(node.type === "mdxJsxFlowElement" ||
				node.type === "mdxJsxTextElement") &&
			node.name === "Card"
		) {
			const line = node.position?.start.line ?? 0;
			const attribute = (key) =>
				node.attributes.find(
					(candidate) =>
						candidate.type === "mdxJsxAttribute" && candidate.name === key,
				);
			const name = attribute("name");
			if (!name || typeof name.value !== "string") {
				throw new Error(
					`${file}:${line}: <Card> needs name="..." written as a string, ` +
						"so the pictures it shows can be read without running the page.",
				);
			}
			const loop = attribute("loop");
			if (loop && loop.value !== null) {
				throw new Error(
					`${file}:${line}: <Card name="${name.value}"> writes loop with a value; ` +
						"write it bare, as the pages do, so it reads the same here and in the component.",
				);
			}
			cards.push({ name: name.value, loop: Boolean(loop) });
		}
		for (const child of node.children ?? []) visit(child);
	};
	visit(tree);
	return cards;
}

/**
 * The files a card shows.
 *
 * @param {{ name: string, loop: boolean }} card
 * @returns {string[]}
 */
function filesOf({ name, loop }) {
	const stems = loop ? [`card-${name}`, `card-${name}-loop`] : [`card-${name}`];
	return stems.flatMap((stem) => [`${stem}.svg`, `${stem}_dark.svg`]);
}

/**
 * Why a picture is not shown, and what would show it.
 *
 * @param {string} file an unreferenced card-*.svg
 * @param {Map<string, string[]>} pagesByName every card name shown, and where
 * @returns {string}
 */
function explain(file, pagesByName) {
	const stem = file.replace(/(?:_dark)?\.svg$/, "");
	const loop = /-loop$/.test(stem);
	const name = stem.replace(/^card-/, "").replace(/-loop$/, "");
	if (loop) {
		const pages = pagesByName.get(name);
		if (pages) {
			return (
				`${file}: <Card name="${name}"> is on ${pages.join(", ")}, but none of them ` +
				`passes loop, which is what shows this picture. Add loop to one of them.`
			);
		}
		return `${file}: no page has <Card name="${name}" loop>, which is what shows this picture.`;
	}
	return `${file}: no page has <Card name="${name}">, which is what shows this picture.`;
}

/**
 * Compares the pictures with the cards the pages show.
 *
 * @param {string[]} assets the card-*.svg file names
 * @param {{ page: string, cards: { name: string, loop: boolean }[] }[]} pages
 * @returns {string[]} one failure per picture no page shows
 */
function unshown(assets, pages) {
	const shown = new Set();
	const pagesByName = new Map();
	for (const { page, cards } of pages) {
		for (const card of cards) {
			for (const file of filesOf(card)) shown.add(file);
			const list = pagesByName.get(card.name) ?? [];
			if (!list.includes(page)) list.push(page);
			pagesByName.set(card.name, list);
		}
	}
	return assets
		.filter((file) => CARD_FILE.test(file) && !shown.has(file))
		.sort()
		.map((file) => explain(file, pagesByName));
}

/* ------------------------------------------------------------------
 * Fixtures
 * ------------------------------------------------------------------ */

const PAIR = (stem) => [`${stem}.svg`, `${stem}_dark.svg`];

const SELF_TESTS = [
	[
		"a plain card shows its two palettes and nothing else",
		() => {
			const pages = [
				{
					page: "a.mdx",
					cards: cardsIn('<Card name="x" alt="X" />\n', "a.mdx"),
				},
			];
			const failures = unshown(
				[...PAIR("card-x"), ...PAIR("card-x-loop")],
				pages,
			);
			if (
				failures.length !== 2 ||
				!failures.every((f) => f.includes("passes loop"))
			) {
				throw new Error(
					`expected the two loop pictures to fail, got ${JSON.stringify(failures)}`,
				);
			}
		},
	],
	[
		"a loop card shows the picture that plays once and the looping one, in both palettes",
		() => {
			const pages = [
				{
					page: "a.mdx",
					cards: cardsIn('<Card name="x" alt="X" animated loop />\n', "a.mdx"),
				},
			];
			const failures = unshown(
				[...PAIR("card-x"), ...PAIR("card-x-loop")],
				pages,
			);
			if (failures.length > 0) throw new Error(JSON.stringify(failures));
		},
	],
	[
		"a card quoted in a code block or a code span is not a card",
		() => {
			const source = [
				"---",
				"title: T",
				"---",
				"",
				"```mdx",
				'<Card name="x" alt="X" />',
				"```",
				"",
				'Write `<Card name="x" alt="X" />` to show it.',
				"",
			].join("\n");
			const cards = cardsIn(source, "a.mdx");
			if (cards.length > 0) throw new Error(JSON.stringify(cards));
		},
	],
	[
		"a picture nobody shows fails by name, with the card that would show it",
		() => {
			const failures = unshown(PAIR("card-y"), [{ page: "a.mdx", cards: [] }]);
			if (failures.length !== 2 || !failures[0].includes('<Card name="y">')) {
				throw new Error(JSON.stringify(failures));
			}
		},
	],
	[
		"a name written as an expression is refused",
		() => {
			try {
				cardsIn('<Card name={"x"} alt="X" />\n', "a.mdx");
			} catch {
				return;
			}
			throw new Error("an expression name was accepted");
		},
	],
	[
		"a page under a locale directory is that locale's, and any other is English",
		() => {
			const got = ["card/index.mdx", "es/card/index.mdx", "index.mdx"].map(
				(page) => localeOf(page),
			);
			if (got.join() !== "en,es,en") throw new Error(got.join());
		},
	],
	[
		"files that are not card pictures are not gated",
		() => {
			const failures = unshown(["logo.svg", "card-x.png"], []);
			if (failures.length > 0) throw new Error(JSON.stringify(failures));
		},
	],
];

const selfTestFailures = [];
for (const [name, run] of SELF_TESTS) {
	try {
		run();
	} catch (error) {
		selfTestFailures.push(`${name}, ${error.message}`);
	}
}
if (selfTestFailures.length > 0) {
	console.error(
		`\n✗ card assets self-test (${selfTestFailures.length} failed)`,
	);
	for (const failure of selfTestFailures) console.error(`    • ${failure}`);
	process.exit(1);
}
if (process.argv.includes("--self-test")) {
	console.log(`✓ card assets self-test: ${SELF_TESTS.length} checks passed.`);
	process.exit(0);
}

/* ------------------------------------------------------------------
 * The corpus
 * ------------------------------------------------------------------ */

const assets = readdirSync(ASSETS_DIR).filter((file) => CARD_FILE.test(file));
let pages;
try {
	pages = listPages(DOCS_DIR).map((page) => ({
		page,
		cards: cardsIn(readFileSync(path.join(DOCS_DIR, page), "utf8"), page),
	}));
} catch (error) {
	console.error(`✗ ${error.message}`);
	process.exit(1);
}

let failed = false;
for (const [locale, language] of Object.entries(LOCALES)) {
	const own = pages.filter(({ page }) => localeOf(page) === locale);
	const failures = unshown(assets, own);
	const count = own.reduce((sum, { cards }) => sum + cards.length, 0);
	if (failures.length > 0) {
		failed = true;
		console.error(
			`✗ ${failures.length} card ${failures.length === 1 ? "picture is" : "pictures are"} ` +
				`in src/assets/ and on no ${language} page:`,
		);
		for (const failure of failures) console.error(`    • ${failure}`);
		continue;
	}
	console.log(
		`✓ ${language}: every one of the ${assets.length} card pictures in src/assets/ ` +
			`is shown by one of the ${count} <Card> invocations in ${own.length} pages.`,
	);
}
if (failed) process.exit(1);
