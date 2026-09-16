/**
 * Rounds a fractional `width` or `height` on an image or picture source to a
 * whole number of pixels.
 *
 * rehype-mermaid's `img-svg` strategy sizes each diagram's `<img>` and its
 * dark `<source>` from the rendered SVG's bounding box, which Chromium measures
 * in fractions: `width="819.90625"`. HTML defines both attributes as valid
 * non-negative integers, so every page with a diagram failed html-validate.
 * The attributes size the box the diagram is drawn in (diagram.css keeps the
 * height automatic), and half a pixel either way in that box is not something
 * a reader can see, so rounding changes nothing on screen and makes the
 * markup valid.
 */
const SIZED = new Set(["img", "source"]);

/** @param {unknown} value */
const rounded = (value) => {
	const number = typeof value === "number" ? value : Number(value);
	return Number.isFinite(number) && !Number.isInteger(number)
		? Math.round(number)
		: value;
};

export default function rehypeIntegerDimensions() {
	/** @param {any} node */
	const walk = (node) => {
		if (!node) return;
		if (node.type === "element" && SIZED.has(node.tagName)) {
			const { properties } = node;
			if (properties?.width !== undefined) {
				properties.width = rounded(properties.width);
			}
			if (properties?.height !== undefined) {
				properties.height = rounded(properties.height);
			}
		}
		if (Array.isArray(node.children)) node.children.forEach(walk);
	};
	// Walked by hand for the reason rehype-tables.mjs gives: the
	// visitor package is only present transitively.
	return (tree) => walk(tree);
}
