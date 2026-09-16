// The command that draws a card, as the page shows it under the picture and as
// the markdown twin writes it.
//
// Generated rather than typed, from the two things that decide which picture a
// page shows: the layout's name and whether the picture is the looping one.
// Both are the props of <Card>, so the page (src/components/Card.astro) and its
// reduction (src/lib/page-markdown.mjs) build the command from the same call,
// and a reader of docs/ or llms-full.txt copies the line a reader of the page
// copies.
//
// It is the command the gallery itself runs (test/e2e/card_gallery_test.go),
// less what is only a default: `-card-motion once` is what the binary does
// without the flag, so the flag appears only for the looping picture. Only a
// layout the registry marks Loops has one, and <Card loop> refuses a layout
// whose looping picture was never generated, so the flag cannot be offered on
// a card it would not change. `-card-layout` stays even for `summary`, the
// default: on a page that exists to choose a layout, the name is the part a
// reader changes.
//
// `-card-theme both` is there because the page shows the card in the palette
// the page is in, which is two files, card.svg and card_dark.svg, and the
// README snippet that shows them (see the card overview) needs both.

/**
 * The binary's command line, on one line and with the part that changes
 * first. One line, because a shell block on this site draws a prompt at the
 * start of every line that does not continue the one before it, and a
 * reader on a phone never sees the backslash that would say so once it has
 * scrolled out of the block; the part that changes first, because at 400
 * pixels that is all of the line there is room for, and the layout's name is
 * what tells two cards' commands apart. Flags are order-free, so the order is
 * the reader's, not the binary's.
 *
 * @param {string} name the layout
 * @param {boolean} loop whether the picture shown is the looping one
 * @returns {string} a shell command
 */
export function cardCommand(name, loop) {
	return `ghchronicle -card-layout ${name} -card-theme both -card-only -card card.svg -config config.yaml${loop ? " -card-motion loop" : ""}`;
}

/**
 * The same card as a step of the composite Action, with the paths the profile
 * README workflow uses (install/actions), so the step drops into that
 * workflow as it is.
 *
 * @param {string} name the layout
 * @param {boolean} loop whether the picture shown is the looping one
 * @returns {string} a workflow step
 */
export function cardStep(name, loop) {
	return [
		"- uses: jmrplens/ghchronicle@v1",
		"  with:",
		"    token: ${{ secrets.GHCHRONICLE_TOKEN }}",
		"    mode: card",
		"    card: generated/card.svg",
		`    card-layout: ${name}`,
		"    card-theme: both",
		...(loop ? ["    card-motion: loop"] : []),
	].join("\n");
}

// The words around the command, per locale. The command itself is not
// translated: a flag is a flag in both languages.
export const CARD_COMMAND_TEXT = {
	en: {
		binary: "Binary",
		action: "GitHub Action",
		fullSize: "Full size",
		fullSizeOf: (name) => ` of the ${name} card`,
		summary: "Command for",
		loop: "The looping picture:",
	},
	es: {
		binary: "Binario",
		action: "GitHub Action",
		fullSize: "Tamaño real",
		fullSizeOf: (name) => ` de la tarjeta ${name}`,
		summary: "Orden para",
		loop: "La imagen en bucle:",
	},
};
