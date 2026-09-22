// What ESLint covers here and why each file that needs an exception gets one.
//
// It stays in site/ because ESLint 10 resolves a flat config from the
// directory of each file rather than from the working directory, and the
// plugins live in site/node_modules. `eslint .` is run from site/.
//
// So its reach is site/, and one file of the repository is outside it:
// scripts/check-doc-links.mjs, which belongs to the repository rather than to
// the site and is formatted by Prettier like everything else. Reaching it
// needs basePath, and with basePath the files: patterns of the two exceptions
// below stop matching, which turns them off without saying so. Moving the
// script into site/scripts would fix the reach and put it where it does not
// belong. Neither trade is worth a single file.
import js from "@eslint/js";
import globals from "globals";
import eslintPluginAstro from "eslint-plugin-astro";
import eslintConfigPrettier from "eslint-config-prettier";

export default [
	{
		ignores: ["dist/", ".astro/"],
	},

	// The .mjs scripts: the site's own build and check scripts. Node's
	// built-in globals and nothing else, so a browser global reached for by
	// habit is an error rather than a runtime surprise.
	{
		files: ["**/*.mjs"],
		languageOptions: {
			ecmaVersion: "latest",
			sourceType: "module",
			globals: globals.nodeBuiltin,
		},
		...js.configs.recommended,
	},

	// check-table-fit measures tables in a real browser: the functions it
	// hands to page.evaluate are serialised, sent over the wire and run
	// inside the page, where document and getComputedStyle are exactly the
	// right things to reach for. They are undefined in the file's own scope
	// and defined where the code actually runs.
	{
		files: ["scripts/check-table-fit.mjs"],
		languageOptions: {
			globals: { ...globals.nodeBuiltin, ...globals.browser },
		},
	},

	// preview.mjs strips the colour codes Astro writes even into a pipe, and
	// an escape sequence is a control character by definition. The rule is
	// right in general and wrong for the one regular expression whose job is
	// to match one.
	{
		files: ["scripts/preview.mjs"],
		rules: { "no-control-regex": "off" },
	},

	...eslintPluginAstro.configs.recommended,
	eslintConfigPrettier,
];
