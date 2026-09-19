package dashboards

// Sections is the dashboard, top to bottom. The order is the reading order: an
// overview, then what is true since the beginning, then one question per
// section, and last what the collector knows about itself.
//
// Every row but the first is collapsed. Open, the seven leading sections
// were twenty-three phone screens before the reader found out there were
// ten more; collapsed, the second screen is the index of the sixteen, each a
// tap away, and Grafana keeps what was opened in the URL. On a desktop it
// costs a click per section and the first render asks for the Overview's
// panels rather than eighty-nine.
var Sections = []Section{
	{"Overview", false, overview},
	{"Lifetime", true, lifetime},
	{"Audience", true, audience},
	{"Stars and forks", true, stars},
	{"Contributions", true, contributions},
	{"Pull requests and issues", true, flow},
	{"Continuous integration", true, ci},
	{"Code", true, code},
	{"Planning and community", true, planning},
	{"Delivery and access", true, delivery},
	{"Releases", true, releases},
	{"Security", true, security},
	{"Cost", true, cost},
	{"Activity", true, activity},
	{"Inventory", true, inventory},
	{"Profile and sponsorship", true, profileSection},
	{"The collector itself", true, collectorSection},
}
