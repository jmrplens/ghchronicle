// The translation keys src/content/i18n/*.json adds on top of Starlight's own.
//
// Starlight types Astro.locals.t from StarlightApp.I18n (see its global.d.ts).
// Declaring ours here is what lets a component call t("ghc.breadcrumb.home")
// with the key checked at compile time; a key added to the JSON files is added
// here too, or the call will not type.
declare namespace StarlightApp {
	interface I18n {
		"ghc.breadcrumb.home": string;
		"ghc.footer.byline": string;
		"ghc.footer.licence": string;
		"ghc.footer.changelog": string;
	}
}
