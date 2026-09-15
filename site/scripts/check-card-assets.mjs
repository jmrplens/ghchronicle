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
 * known without evaluating the page, and a `<Card>` written in a `.md` page is
 * a hard error for the reason the same gate gives: markdown is not MDX, Astro
 * emits the tag as raw HTML and it renders nothing, so a page that looks like a
 * showcase shows no card at all.
 *
 * A missing directory, an empty one and a corpus with no pages are all
 * failures, not passes. "Every one of the 0 card pictures is shown" is exactly
 * how a path drift or a wrong working directory would read as success.
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

import { localeOf } from "../src/lib/site.mjs";

const DOCS_DIR = fileURLToPath(new URL("../src/content/docs", import.meta.url));
const ASSETS_DIR = fileURLToPath(new URL("../src/assets", import.meta.url));
const CARD_FILE = /^card-.+\.svg$/;
// How each locale is named in a failure. `localeOf` decides which one a page is
// in, and it is the site's own, imported rather than restated: a fourth copy of
// "a path under es/ is Spanish" is a fourth place to forget when a locale is
// added. Which locales are gated is not stated here either, it is read off the
// corpus, so a locale the site grows is gated the moment it has a page. A name
// missing from this map falls back to the locale's own code.
const LANGUAGES = { en: "English", es: "Spanish" };

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
	// `.mdx` gets the MDX extensions and `.md` does not, which mirrors how Astro
	// treats the two.
	const mdx = file.endsWith(".mdx");
	const tree = fromMarkdown(body, {
		extensions: mdx ? [mdxjs()] : [],
		mdastExtensions: mdx ? [mdxFromMarkdown()] : [],
	});
	const cards = [];
	const visit = (node) => {
		// A `.md` page is markdown, not MDX: Astro passes `<Card … />` through as
		// raw HTML, the browser makes an unknown element of it and nothing
		// renders. It therefore cannot be counted as showing a picture, and it
		// must not be ignored either, or the page would look like a showcase and
		// the gate would stay green on a card nobody can see. The same case is a
		// hard error in check-i18n-parity.mjs, and the two gates say the same
		// thing about it. Only `html` nodes are read, so a tag in a code fence or
		// a code span cannot reach here, and comment spans are blanked first,
		// because markdown has no comment node.
		if (!mdx && node.type === "html" && typeof node.value === "string") {
			const visible = node.value.replace(/<!--[\s\S]*?(?:-->|$)/g, " ");
			if (/<\/?Card\b/.test(visible)) {
				throw new Error(
					`${file}:${node.position?.start.line ?? 0}: <Card> is written in a ` +
						".md page, where it renders nothing. Rename the page to .mdx and " +
						"import the component, or remove the tag.",
				);
			}
		}
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
			const one = pages.length === 1;
			return (
				`${file}: <Card name="${name}"> is on ${pages.join(", ")}, ` +
				`but ${one ? "it does not pass" : "none of them passes"} loop, which is ` +
				`what shows this picture. Add loop to ${one ? "it" : "one of them"}.`
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
				!failures.every((f) => f.includes("does not pass loop"))
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
		"the message says which pages hold the name, in the number it finds them",
		() => {
			const card = cardsIn('<Card name="x" alt="X" />\n', "a.mdx");
			const one = unshown(PAIR("card-x-loop"), [
				{ page: "a.mdx", cards: card },
			]);
			const two = unshown(PAIR("card-x-loop"), [
				{ page: "a.mdx", cards: card },
				{ page: "b.mdx", cards: card },
			]);
			if (
				!one[0].includes("it does not pass loop") ||
				!one[0].endsWith("Add loop to it.")
			) {
				throw new Error(one[0]);
			}
			if (!two[0].includes("a.mdx, b.mdx, but none of them passes loop")) {
				throw new Error(two[0]);
			}
		},
	],
	[
		"a card written in a .md page renders nothing, and is a hard error",
		() => {
			try {
				cardsIn('<Card name="x" alt="X" />\n', "a.md");
			} catch (error) {
				if (!error.message.includes("renders nothing")) throw error;
				return;
			}
			throw new Error("a component tag in a .md page was accepted");
		},
	],
	[
		"a card commented out in a .md page is not one",
		() => {
			const cards = cardsIn('<!-- <Card name="x" alt="X" /> -->\n', "a.md");
			if (cards.length > 0) throw new Error(JSON.stringify(cards));
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

let assets;
let pages;
// A missing directory is the failure this gate is least able to survive and the
// one it must not narrate with a stack trace: both reads are here, so a wrong
// path, a wrong working directory or a partial checkout is reported in the same
// voice as a card nobody shows.
try {
	assets = readdirSync(ASSETS_DIR).filter((file) => CARD_FILE.test(file));
	pages = listPages(DOCS_DIR).map((page) => ({
		page,
		cards: cardsIn(readFileSync(path.join(DOCS_DIR, page), "utf8"), page),
	}));
} catch (error) {
	console.error(`✗ card assets: ${error.message}`);
	// A directory that is not there reads the same as one that is empty: the
	// gate examined nothing, and the reason is almost always where it was told
	// to look.
	if (error.code === "ENOENT") {
		console.error(
			"  Check the path and the working directory; this is not a pass.",
		);
	}
	process.exit(1);
}

// A gate that reports success on an empty corpus is worse than no gate: a path
// drift, a partial checkout or a wrong cwd would turn it permanently green, and
// this one would say so in words, "every one of the 0 card pictures is shown".
// Both floors are needed: no pictures means nothing was gated, and no pages
// means nothing could show them.
if (assets.length === 0) {
	console.error(
		`✗ card assets: no card-*.svg found in ${ASSETS_DIR}, nothing was gated.`,
	);
	console.error(
		"  Check the path and the working directory; this is not a pass.",
	);
	process.exit(1);
}
if (pages.length === 0) {
	console.error(
		`✗ card assets: no pages found under ${DOCS_DIR}, nothing shows anything.`,
	);
	console.error(
		"  Check the path and the working directory; this is not a pass.",
	);
	process.exit(1);
}

// The locales to gate are the ones the corpus has pages in, so a locale the
// site grows needs no edit here, and one that loses every page fails rather
// than disappearing from the report.
const locales = [...new Set(pages.map(({ page }) => localeOf(page)))].sort();

let failed = false;
for (const locale of locales) {
	const language = LANGUAGES[locale] ?? locale;
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
