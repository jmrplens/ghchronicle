// @ts-check
/**
 * The questions a page asks in its own headings, and the answer to each, for
 * the FAQPage node Head.astro emits on a page whose frontmatter sets `faq`.
 *
 * Read from the page body rather than from a list in the frontmatter, so the
 * structured data cannot say something the page does not: the question is the
 * `##` heading as written, and the answer is the first paragraph under it,
 * which the page is written to make self-contained. A heading that does not end
 * in a question mark is a section, not a question, and is left out.
 *
 * The body is MDX source, and this reads it without MDX. It removes only what
 * it can remove exactly (a link's target, a code span's backticks, bold), and
 * anything else that is markup rather than text stops the build, naming the
 * page and the question: a `{stats.panels}` the page renders as a number, an
 * aside, an italic, an entity or an escape would otherwise reach the JSON-LD as
 * written, which is the leak resolveData in page-markdown.mjs closes for the
 * twins. scripts/check-structured-data.mjs then holds each question and answer
 * to the heading and paragraph the build rendered.
 */

/** The version token remark-version.mjs substitutes on the page, code included. */
const VERSION_TOKEN = "__VERSION__";

// Stand-ins for code spans while the prose around them is read, in the Private
// Use Area so that no page can contain one.
const OPEN = "\uE000";
const CLOSE = "\uE001";

/** A code span's stand-in, and the index of what it stood for. */
const STAND_IN = new RegExp(`${OPEN}(\\d+)${CLOSE}`, "g");

/**
 * What is still markup in a stretch of prose, and what to call it. Code spans
 * are out of the prose by then: on the page they are literal, braces and angle
 * brackets included, so what they hold is text.
 *
 * @type {[RegExp, string][]}
 */
const LEFTOVERS = [
	[/\\/, "a backslash escape or line break"],
	[/[{}]/, "an MDX expression"],
	[/</, "JSX or HTML"],
	[/:::/, "an aside directive"],
	[/!\[/, "an image"],
	[/[[\]]/, "a link this reads no target for"],
	[/\*/, "emphasis"],
	// Intraword underscores (snake_case) are text to CommonMark; one at the edge
	// of a word can open or close emphasis.
	[/(?<![\p{L}\p{N}_])_|_(?![\p{L}\p{N}_])/u, "emphasis"],
	[/(~{1,2})(?=\S)[^~]*?\S\1/, "a strikethrough"],
	[/&(?:#\d+|#x[\da-f]+|[a-z][a-z\d]*);/i, "a character reference"],
	// Smartypants sets it as an em dash on the page, and this project writes
	// none; the curly quotes it sets are compared straight by the check.
	[/--/, "a double hyphen"],
];

/**
 * A line that begins a block other than a paragraph, first under a heading or
 * inside the paragraph, where it ends the paragraph the source seems to
 * continue. Written wider than CommonMark's rules, since a false alarm costs a
 * rewording and a miss costs an answer the page does not give.
 */
const BLOCK_START =
	/^\s*(?:[-*+](?:\s|$)|\d+[.)](?:\s|$)|>|#{1,6}(?:\s|$)|```|~~~|:::|<|\{|\||import\s|export\s|=+\s*$|-+\s*$)|^(?: {4}|\t)/;

/**
 * The text a line of markdown renders to, or a throw when part of it would
 * reach the JSON-LD as markup.
 *
 * @param {string} markdown
 * @param {string} where the page and question, named in the failure
 * @returns {string}
 */
function plain(markdown, where) {
	if (markdown.includes(VERSION_TOKEN)) {
		throw new Error(
			`${where} holds ${VERSION_TOKEN}, which the page renders as the ` +
				"release number and the FAQPage would publish as written.",
		);
	}
	/** @type {string[]} */
	const code = [];
	// A span opens and closes with runs of the same length, and CommonMark drops
	// one space from each end when both are there.
	const prose = markdown
		.replace(/(`+)(?!`)([\s\S]*?[^`])\1(?!`)/g, (_, _fence, content) => {
			const text = content.replace(/\s+/g, " ");
			code.push(/^ .* $/.test(text) && text.trim() ? text.slice(1, -1) : text);
			return `${OPEN}${code.length - 1}${CLOSE}`;
		})
		.replace(/(?<!!)\[([^\]]+)\]\([^)\s]+\)/g, "$1")
		.replace(/\*\*([^*]+)\*\*/g, "$1");
	for (const [pattern, what] of LEFTOVERS) {
		const found = pattern.exec(prose);
		if (!found) continue;
		// Thirty characters either side, widened to whole code spans.
		let start = Math.max(0, found.index - 30);
		let end = found.index + 30;
		const opened = prose.lastIndexOf(OPEN, start);
		if (opened !== -1 && prose.indexOf(CLOSE, opened) >= start) start = opened;
		const last = prose.lastIndexOf(OPEN, end - 1);
		if (last !== -1 && prose.indexOf(CLOSE, last) >= end) {
			end = prose.indexOf(CLOSE, last) + 1;
		}
		const context = prose
			.slice(start, end)
			.replace(STAND_IN, (_, index) => `\`${code[index]}\``)
			.replace(/\s+/g, " ");
		throw new Error(
			`${where} still holds ${what} after reduction ("...${context}..."). ` +
				"The FAQPage would publish it as written while the page renders it; " +
				"reword it as plain text, or put a literal in a code span.",
		);
	}
	return prose
		.replace(STAND_IN, (_, index) => code[index])
		.replace(/\s+/g, " ")
		.trim();
}

/**
 * @param {string} body the page's MDX body, frontmatter removed
 * @param {string} file named in the failure
 * @returns {{ question: string, answer: string }[]}
 */
export function faqEntries(body, file) {
	const entries = [];
	const lines = body.split("\n");
	for (let i = 0; i < lines.length; i++) {
		const heading = /^## (.+\?)\s*$/.exec(lines[i]);
		if (!heading) continue;
		const where = `${file}: "${heading[1]}"`;
		let j = i + 1;
		while (j < lines.length && lines[j].trim() === "") j++;
		const paragraph = [];
		while (j < lines.length && lines[j].trim() !== "") {
			if (BLOCK_START.test(lines[j])) {
				throw new Error(
					paragraph.length === 0
						? `${where} has no paragraph under it to answer it with; ` +
								"an FAQ answer is the first paragraph after the question."
						: `${where} is answered by a paragraph that line ${j + 1} of the body ` +
								`("${lines[j].trim()}") ends early, so the answer would ` +
								"not be the paragraph the page renders. Put a blank line " +
								"before it, or reword the line.",
				);
			}
			paragraph.push(lines[j]);
			j++;
		}
		const answer = plain(paragraph.join("\n"), where);
		if (!answer) {
			throw new Error(
				`${where} has no paragraph under it to answer it with; ` +
					"an FAQ answer is the first paragraph after the question.",
			);
		}
		entries.push({ question: plain(heading[1], where), answer });
	}
	if (entries.length === 0) {
		throw new Error(
			`${file} sets faq but has no "## ...?" heading to read a question from.`,
		);
	}
	return entries;
}
