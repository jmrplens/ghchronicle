// The landing page, as markdown.
//
// The two landings are a component fed from src/data/home.ts, so unlike every
// other page they have no prose body to reduce: their MDX is one <Home /> tag.
// Rendered from the same typed object the component renders, the twin says
// what the page says, in the same order, in both locales, and stays that way
// when the copy is edited, because there is only one copy to edit.
import stats from "../data/stats.json";

/**
 * The few inline HTML tags the copy uses for emphasis, as markdown. The strings
 * are written for a component that renders them as HTML, so a twin that left
 * them alone would show tags to a reader who came for text.
 *
 * @param {string} text a copy string
 * @returns {string} the same text, with inline HTML resolved
 */
const inline = (text) =>
	text
		.replace(/<\/?strong>/g, "**")
		.replace(/<\/?em>/g, "_")
		.replace(/<\/?code>/g, "`");

/**
 * The landing page's markdown, from the same content object the page renders.
 *
 * @param {import("../data/home").HomeContent} content the locale's copy
 * @returns {string} markdown, without the title and description the twin adds
 */
export function renderHome(content) {
	const lines = [];
	/** One block, followed by the blank line markdown needs after it. */
	const push = (block) => lines.push(block, "");
	/** A list, which is one block even though it is several lines. */
	const list = (items) => push(items.join("\n"));

	push(inline(content.claim));

	push(`## ${content.statsLabel}`);
	list(
		[
			["measurements", stats.measurements],
			["families", stats.families],
			["sinks", stats.sinks],
			["panels", stats.panels],
		].map(
			([key, value]) =>
				`- [${value} ${content.statLabels[key]}](${content.statHrefs[key]})`,
		),
	);

	push(`## ${content.what.title}`);
	for (const paragraph of content.what.body) push(inline(paragraph));

	for (const section of [content.who, content.honest]) {
		push(`## ${section.title}`);
		list(section.items.map((item) => `- ${inline(item)}`));
	}

	const { proof } = content;
	push(`## ${proof.title}`);
	push(inline(proof.lead));
	for (const [title, note, code, language] of [
		[proof.installTitle, proof.installNote, proof.install, "sh"],
		[proof.configTitle, proof.configNote, proof.config, "yaml"],
		[proof.outputTitle, proof.outputNote, proof.output, "text"],
	]) {
		push(`### ${title}`);
		push(inline(note));
		push(`\`\`\`${language}\n${code}\n\`\`\``);
	}

	push(`## ${content.shape.title}`);
	push(inline(content.shape.lead));
	list(
		content.shape.steps.map(
			(step, index) => `${index + 1}. **${step.title}**: ${inline(step.body)}`,
		),
	);

	push(`## ${content.start.title}`);
	push(inline(content.start.lead));
	list(
		content.start.links.map(
			(link) => `- [${link.text}](${link.href}): ${inline(link.note)}`,
		),
	);

	return lines
		.join("\n")
		.replace(/\n{3,}/g, "\n\n")
		.trim();
}
