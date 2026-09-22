// What ESLint covers here: the .astro components, through
// eslint-plugin-astro, with eslint-config-prettier turning off the rules that
// would argue with the formatter.
//
// It deliberately does not carry js.configs.recommended, so the .mjs scripts
// under site/scripts and scripts/ are formatted by Prettier and checked by
// nothing else. Adding JavaScript rules is a decision to make on its own, not
// a side effect of tidying up scopes.
//
// ESLint 10 resolves a flat config from the directory of each file rather than
// from the working directory, and the plugins live in site/node_modules, so
// this file stays here and `eslint .` is run from site/.
import eslintPluginAstro from "eslint-plugin-astro";
import eslintConfigPrettier from "eslint-config-prettier";

export default [
	...eslintPluginAstro.configs.recommended,
	eslintConfigPrettier,
	{
		ignores: ["dist/", ".astro/"],
	},
];
