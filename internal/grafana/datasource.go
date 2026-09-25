package grafana

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Datasource is the part of a Grafana datasource this needs in order to create
// one and to tell an existing one apart from the one it would have made.
type Datasource struct {
	UID      string
	Name     string
	Type     string
	URL      string
	Database string
	// User is the account the datasource connects as, which only the SQL
	// datasources have: everything else carries its credential in a header.
	User string
	// JSON is the plugin's own settings: the HTTP method, the query language
	// version, the name of a header. It differs per store, so it is the
	// caller's to fill.
	JSON map[string]any
	// Secret is written and never read back: Grafana reports only which keys
	// are set, never their values. That is why a datasource carrying one is
	// written on every reconcile rather than compared first.
	Secret map[string]string
}

// datasourceByUID is the path a datasource answers to, which three calls here
// need and one of them builds a suffix onto.
const datasourceByUID = "/api/datasources/uid/"

// Outcome says what reconciling did, for a caller that reports it to a person.
type Outcome int

// The four things that can happen to a datasource.
const (
	Unchanged Outcome = iota
	Created
	Updated
	// Adopted is a datasource Grafana already had under a uid of its own,
	// saying everything the one asked for would say, used as it is instead
	// of being joined by a second.
	Adopted
)

// String names the outcome the way a log line wants it.
func (o Outcome) String() string {
	switch o {
	case Created:
		return "created"
	case Updated:
		return "updated"
	case Adopted:
		return "adopted"
	default:
		return "unchanged"
	}
}

// RefusedError is Grafana turning the token away with a 403. It is a type of
// its own because a caller can act on it where it cannot act on a server that
// is down: the permission the call needed has a name, and so, often, does a
// setting that avoids needing it.
type RefusedError struct {
	// Call is what was asked, in the words an error message wants.
	Call string
	// Permission is the action the call needs. It is the one this package
	// knows the call to need rather than one read out of the answer: Grafana
	// words its refusal as prose, and every refusal measured on Grafana 13
	// named the same action this does.
	Permission string
	// Message is Grafana's own answer.
	Message string
}

func (e *RefusedError) Error() string { return e.Call + ": " + e.Message }

// failure is the error for an answer that is not the one a call wanted: a
// RefusedError for a 403, the plain message for anything else.
func failure(call, permission string, res Response) error {
	if res.Status == http.StatusForbidden {
		return &RefusedError{Call: call, Permission: permission, Message: answerText(res)}
	}
	return fmt.Errorf("%s: %s", call, answerText(res))
}

// EnsureDatasource makes the datasource at want.UID be the one described,
// creating it when it is not there and correcting it when it is there and
// differs. It is the whole of what "give it the Grafana and the sink and it
// sets itself up" means, so it is deliberately the only way in for a
// datasource a dashboard cannot do without.
func (c Client) EnsureDatasource(ctx context.Context, want Datasource,
	timeout time.Duration,
) (Outcome, error) {
	_, outcome, err := c.ensure(ctx, want, false, timeout)
	return outcome, err
}

// EnsureDatasourceOrAdopt is EnsureDatasource for a datasource that its type,
// its address and the settings Grafana shows describe completely. When want.UID
// is not there and Grafana already has a datasource that says all of that under
// a uid of its own, that one is used as it is rather than a second one being
// made beside it. It returns the uid to read from: want.UID, or the adopted
// one.
//
// Only a datasource this has not made yet is looked for elsewhere. Once
// want.UID exists it is the one kept and corrected, so a datasource somebody
// adds later at the same address never takes over from it.
//
// A datasource that needs a secret is never adopted, since Grafana never hands
// one back to compare. Beyond that, the caller decides whether a datasource is
// nothing more than what Grafana shows, which is why this is a second way in
// rather than a flag on the first.
func (c Client) EnsureDatasourceOrAdopt(ctx context.Context, want Datasource,
	timeout time.Duration,
) (string, Outcome, error) {
	return c.ensure(ctx, want, true, timeout)
}

// ensure is both of the above. adopt says whether a datasource already there
// under another uid may stand in for a missing want.UID.
func (c Client) ensure(ctx context.Context, want Datasource, adopt bool,
	timeout time.Duration,
) (string, Outcome, error) {
	if want.UID == "" || want.Type == "" {
		return "", Unchanged, errors.New("a datasource needs a uid and a type")
	}
	found, err := c.Do(ctx, http.MethodGet, datasourceByUID+want.UID, nil, timeout)
	if err != nil {
		return "", Unchanged, err
	}
	if found.Status == http.StatusNotFound {
		if adopt {
			if uid := c.sameElsewhere(ctx, want, timeout); uid != "" {
				return uid, Adopted, nil
			}
		}
		return want.UID, Created, c.writeDatasource(ctx, http.MethodPost,
			"/api/datasources", want, timeout)
	}
	if found.Status != http.StatusOK {
		return "", Unchanged, failure("reading datasource "+want.UID,
			"datasources:read", found)
	}
	// A datasource with a secret is written every time. There is no way to
	// tell one carrying the current token from one carrying the token it was
	// given a month ago, so comparing only what can be seen would leave a
	// rotated credential in place for as long as nothing else about the
	// datasource changed. The alternative, a fingerprint of the secret kept in
	// jsonData, buys one saved request on a path that runs at most once per
	// process start and pays for it by putting something derived from a
	// credential in a field every viewer of the datasource can read.
	if len(want.Secret) == 0 && sameDatasource(found.Body, want) {
		return want.UID, Unchanged, nil
	}
	return want.UID, Updated, c.writeDatasource(ctx, http.MethodPut,
		datasourceByUID+want.UID, want, timeout)
}

// sameElsewhere is the uid of a datasource Grafana already has that says what
// want would, under a uid of its own, or "" when there is none or the list
// could not be read.
func (c Client) sameElsewhere(ctx context.Context, want Datasource,
	timeout time.Duration,
) string {
	// A secret is never handed back, and it is exactly where two datasources
	// at one address differ when they differ at all: another tenant, another
	// credential with other rights. Nothing visible tells those apart, so a
	// datasource that needs one is always made.
	if len(want.Secret) > 0 {
		return ""
	}
	all, err := c.Datasources(ctx, timeout)
	if err != nil {
		// A list that was refused or failed is no reason to stop: the create
		// that follows is what ran before anything looked, and it reports its
		// own refusal.
		return ""
	}
	var uids []string
	for _, have := range all {
		if have.UID != "" && have.UID != want.UID && sameAddress(have.URL, want.URL) &&
			sameSettings(have, want) {
			uids = append(uids, have.UID)
		}
	}
	if len(uids) == 0 {
		return ""
	}
	// Two that would both do: the same one every run, so a dashboard is not
	// repointed back and forth between them by consecutive restarts.
	slices.Sort(uids)
	return uids[0]
}

// Datasources is every datasource the token can see, in the part this package
// reads. It needs datasources:read and nothing more, which a token scoped to
// publishing dashboards has where it has nothing else.
func (c Client) Datasources(ctx context.Context, timeout time.Duration) ([]Datasource, error) {
	res, err := c.Do(ctx, http.MethodGet, "/api/datasources", nil, timeout)
	if err != nil {
		return nil, err
	}
	if res.Status != http.StatusOK {
		return nil, failure("listing datasources", "datasources:read", res)
	}
	out := make([]Datasource, 0, len(res.List))
	for _, raw := range res.List {
		if m, ok := raw.(map[string]any); ok {
			out = append(out, fromAPI(m))
		}
	}
	return out, nil
}

// fromAPI reads a datasource out of the shape the API answers in, for both the
// one-datasource and the list endpoints, which agree on these fields.
func fromAPI(m map[string]any) Datasource {
	settings, _ := m["jsonData"].(map[string]any)
	return Datasource{
		UID: field(m, "uid"), Name: field(m, "name"), Type: field(m, "type"),
		URL: field(m, "url"), Database: field(m, "database"), User: field(m, "user"),
		JSON: settings,
	}
}

// writeDatasource posts or puts the body both calls share.
func (c Client) writeDatasource(ctx context.Context, method, path string,
	want Datasource, timeout time.Duration,
) error {
	res, err := c.Do(ctx, method, path, datasourceBody(want), timeout)
	if err != nil {
		return err
	}
	if res.Status >= 200 && res.Status <= 299 {
		return nil
	}
	if method == http.MethodPost {
		return failure("creating datasource "+want.UID, "datasources:create", res)
	}
	return failure("updating datasource "+want.UID, "datasources:write", res)
}

// datasourceBody is the datasource in the shape the API takes. Access is
// always "proxy": the browser reaching a store directly is the other mode, and
// a store this collector writes to is not one a browser can be assumed to
// reach at all.
func datasourceBody(want Datasource) map[string]any {
	settings := map[string]any{}
	maps.Copy(settings, want.JSON)
	body := map[string]any{
		"uid":      want.UID,
		"name":     want.Name,
		"type":     want.Type,
		"url":      want.URL,
		"access":   "proxy",
		"jsonData": settings,
	}
	if want.Database != "" {
		// Both places. The top-level field is what older Grafana reads and
		// what its API still returns; the PostgreSQL plugin has moved the
		// same value into jsonData, and a datasource with only one of the two
		// works on one version and draws nothing on the other.
		body["database"] = want.Database
		settings["database"] = want.Database
	}
	if want.User != "" {
		body["user"] = want.User
	}
	if len(want.Secret) > 0 {
		body["secureJsonData"] = want.Secret
	}
	return body
}

// sameDatasource asks whether what is there already says what this would say.
// Only the fields this writes are compared: a reader who set a timeout or a
// description of their own keeps it, because nothing here has an opinion about
// it and overwriting what was not asked about is how a reconciler becomes a
// thing people turn off.
func sameDatasource(have map[string]any, want Datasource) bool {
	found := fromAPI(have)
	return found.URL == want.URL && found.Name == want.Name && sameSettings(found, want)
}

// sameSettings compares everything but the uid, the name and the address,
// which are the three fields that tell a datasource this made from one it
// found: the type, the database, the user and every plugin setting this
// would write.
func sameSettings(have, want Datasource) bool {
	if have.Type != want.Type {
		return false
	}
	if want.Database != "" && have.Database != want.Database {
		return false
	}
	if want.User != "" && have.User != want.User {
		return false
	}
	for key, value := range want.JSON {
		if fmt.Sprint(have.JSON[key]) != fmt.Sprint(value) {
			return false
		}
	}
	return true
}

// sameAddress says whether two datasource addresses name one server the way a
// person would read them: a trailing slash, or a scheme or host written in
// capitals, does not make it another. An address with no host to compare,
// such as the host:port a PostgreSQL datasource takes, is compared as written,
// since url.Parse reads "db:5432" as a scheme and an opaque "5432" and would
// otherwise find every port on that host the same.
func sameAddress(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	pa, errA := url.Parse(a)
	pb, errB := url.Parse(b)
	if errA != nil || errB != nil || pa.Host == "" || pb.Host == "" {
		return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
	}
	return strings.EqualFold(pa.Scheme, pb.Scheme) &&
		strings.EqualFold(pa.Host, pb.Host) &&
		pa.User.String() == pb.User.String() &&
		strings.TrimRight(pa.Path, "/") == strings.TrimRight(pb.Path, "/") &&
		pa.RawQuery == pb.RawQuery
}

// DatasourceHealth asks the datasource whether it can reach what it names.
// A datasource Grafana accepts is not a datasource that answers: the address
// the collector writes to and the address Grafana reaches it by are often not
// the same one, and without this the first sign of that is a wall of panels
// drawing nothing.
//
// The second return is a message worth printing whatever the first says.
func (c Client) DatasourceHealth(ctx context.Context, uid string,
	timeout time.Duration,
) (ok bool, message string, err error) {
	res, err := c.Do(ctx, http.MethodGet, datasourceByUID+uid+"/health", nil, timeout)
	if err != nil {
		return false, "", err
	}
	// A token that may not query the datasource is not a datasource that
	// cannot reach its store, and reporting it as one sends the reader to fix
	// an address that was right. Measured on Grafana 13.2.1 with a service
	// account that has no role: 403, datasources:query.
	if res.Status == http.StatusForbidden {
		return false, "", failure("asking datasource "+uid+" whether it answers",
			"datasources:query", res)
	}
	// Not every plugin implements it. Saying so is the honest answer; calling
	// an absent check a pass would be the one thing this is here to prevent.
	if res.Status == http.StatusNotFound || res.Status == http.StatusNotImplemented {
		return true, "this datasource plugin has no health check, so it was not tested", nil
	}
	status := field(res.Body, "status")
	message = field(res.Body, "message")
	if message == "" {
		message = answerText(res)
	}
	return status == "OK", message, nil
}

// field reads a string out of a decoded object, and says "" for anything that
// is not one, including a key that is not there. The package already has a
// text() that stringifies one value; this one goes looking for it.
func field(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// answerText is the most useful sentence in a reply that did not go well.
func answerText(res Response) string {
	if message := field(res.Body, "message"); message != "" {
		return Trim(message, 200)
	}
	return fmt.Sprintf("HTTP %d", res.Status)
}
