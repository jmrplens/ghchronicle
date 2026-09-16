// The facts the layouts page states under each layout's own heading: the
// family, the motion, the width and the fields drawn when nothing is asked for.
//
// None of them is typed. `src/data/layouts.json` is the registry in
// `internal/render/layouts.go`, exported by `cmd/gen_layouts`, and
// `make check-layouts` fails when the file no longer matches it. This module
// turns one of those entries into the four short rows a reader sees, in the
// page's own language, and it is shared by the component that renders them
// (`src/components/LayoutFacts.astro`) and by the reduction that writes the
// markdown twin and docs/ (`src/lib/page-markdown.mjs`), the way
// `card-command.mjs` is shared: a twin that stated a different width from the
// page would be a twin nobody had read.
//
// The identifiers stay as they are in both languages. A family is called
// chronicle in Spanish too, and `top_repos` is what a reader types after
// `-card-fields`; translating either would be translating an argument. The
// labels around them are the part that is prose, so they are here.
import layouts from "../data/layouts.json" with { type: "json" };

/** The wording of each row, per locale. */
export const LAYOUT_FACTS_TEXT = {
	en: {
		family: "Family",
		motion: "Motion",
		width: "Width",
		fields: "Default fields",
		still: "still",
		once: "plays once",
		loop: "plays once, or in a loop",
		/** @param {number} width @param {number} min */
		size: (width, min) =>
			min > 0 ? `${width} px, minimum ${min}` : `${width} px`,
		content: "follows its content",
	},
	es: {
		family: "Familia",
		motion: "Movimiento",
		width: "Ancho",
		fields: "Campos por omisión",
		still: "quieto",
		once: "se reproduce una vez",
		loop: "se reproduce una vez, o en bucle",
		/** @param {number} width @param {number} min */
		size: (width, min) =>
			min > 0 ? `${width} px, mínimo ${min}` : `${width} px`,
		content: "sigue al contenido",
	},
};

/**
 * One registry entry, by name.
 *
 * @param {string} name the layout's name
 * @returns {{ name: string, family: string, animated: boolean, loops: boolean, width: number, minWidth: number, fields: string[] } | undefined}
 */
export const layoutNamed = (name) =>
	layouts.find((entry) => entry.name === name);

/** Every registered layout, in registry order, for a failure that lists them. */
export const layoutNames = () => layouts.map((entry) => entry.name);

/**
 * The rows the page shows for one layout.
 *
 * `code` says whether the values are identifiers, which the page sets in code
 * and the markdown twin puts in backticks: a field name is typed after
 * `-card-fields`, a family name is read in a sentence.
 *
 * @param {string} name the layout's name
 * @param {"en" | "es"} locale the page's language
 * @returns {{ label: string, values: string[], code: boolean }[]}
 * @throws {Error} when no layout of that name is registered
 */
export function factsOf(name, locale) {
	const layout = layoutNamed(name);
	if (!layout) {
		throw new Error(
			`"${name}" is not a registered card layout. src/data/layouts.json ` +
				`holds ${layoutNames().join(", ")}; it is generated from ` +
				"internal/render by `make layouts`.",
		);
	}
	const text = LAYOUT_FACTS_TEXT[locale];
	let motion = text.still;
	if (layout.animated) motion = layout.loops ? text.loop : text.once;
	return [
		{ label: text.family, values: [layout.family], code: false },
		{ label: text.motion, values: [motion], code: false },
		{
			label: text.width,
			// A width of zero is the registry saying the layout sets none:
			// badge-row is a row of pills, and stretching it to a fixed width
			// would put gaps in it.
			values: [
				layout.width > 0
					? text.size(layout.width, layout.minWidth)
					: text.content,
			],
			code: false,
		},
		{ label: text.fields, values: layout.fields, code: true },
	];
}
