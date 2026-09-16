// Marks the lines of a code block that continue the line before them.
//
// A shell block on this site is drawn as a terminal, and code.css puts a
// prompt glyph at the start of every line of one, which is right for a line
// that starts a command and wrong for a line that goes on with it: a
// `docker run ... \` spread over five lines read as five commands, and on a
// phone, where the backslash that joins them has scrolled out of the block,
// nothing on screen said otherwise. Whether a line continues another is a
// fact about the text before it, which a stylesheet cannot see, so it is
// decided here, while Expressive Code renders the block, and code.css styles
// from the class.
//
// A line continues the previous one when that line ends with a backslash,
// the shell's own rule. The class is added to every block whatever its
// language; only the terminal frame's prompt reads it.
import { addClassName, definePlugin } from "@astrojs/starlight/expressive-code";

/** The class a continuation line carries. Named in code.css. */
export const CONTINUATION_CLASS = "continues";

/**
 * @param {string | undefined} previous the text of the line before, if any
 * @returns {boolean} whether the line after it continues it
 */
export const continuesAfter = (previous) =>
	previous !== undefined && /\\\s*$/.test(previous);

/** @returns {import("@astrojs/starlight/expressive-code").ExpressiveCodePlugin} the plugin */
export function pluginContinuationLines() {
	return definePlugin({
		name: "Continuation lines",
		hooks: {
			postprocessRenderedLine: ({ codeBlock, lineIndex, renderData }) => {
				// The first line continues nothing, and getLine refuses an index
				// below zero rather than answering with nothing.
				if (lineIndex === 0) return;
				if (continuesAfter(codeBlock.getLine(lineIndex - 1)?.text)) {
					addClassName(renderData.lineAst, CONTINUATION_CLASS);
				}
			},
		},
	});
}
