package grafana

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"
)

// fingerprintField is where the reconciler records which secret a datasource
// was last given. Grafana never hands a secret back, only a flag saying one is
// set, so a token rotated in the configuration while every visible field
// stayed the same would otherwise be invisible and the datasource would go on
// using the old one. A labeled SHA-256, kept to twelve hex characters: enough
// to tell two tokens apart, and not the token.
const fingerprintField = "ghchronicleSecretFingerprint"

// secretLabel keeps the fingerprint from matching the same string hashed for
// any other purpose.
const secretLabel = "ghchronicle datasource secret\x00"

// Datasource is the part of a Grafana datasource this needs in order to create
// one and to tell an existing one apart from the one it would have made.
type Datasource struct {
	UID      string
	Name     string
	Type     string
	URL      string
	Database string
	// JSON is the plugin's own settings: the HTTP method, the query language
	// version, the name of a header. It differs per store, so it is the
	// caller's to fill.
	JSON map[string]any
	// Secret is written and never read back. Grafana reports only which keys
	// are set, which is why the fingerprint exists.
	Secret map[string]string
}

// Outcome says what reconciling did, for a caller that reports it to a person.
type Outcome int

// The three things that can happen to a datasource.
const (
	Unchanged Outcome = iota
	Created
	Updated
)

// String names the outcome the way a log line wants it.
func (o Outcome) String() string {
	switch o {
	case Created:
		return "created"
	case Updated:
		return "updated"
	default:
		return "unchanged"
	}
}

// EnsureDatasource makes the datasource at want.UID be the one described,
// creating it when it is not there and correcting it when it is there and
// differs. It is the whole of what "give it the Grafana and the sink and it
// sets itself up" means, so it is deliberately the only way in.
func (c Client) EnsureDatasource(ctx context.Context, want Datasource,
	timeout time.Duration,
) (Outcome, error) {
	if want.UID == "" || want.Type == "" {
		return Unchanged, errors.New("a datasource needs a uid and a type")
	}
	found, err := c.Do(ctx, http.MethodGet, "/api/datasources/uid/"+want.UID, nil, timeout)
	if err != nil {
		return Unchanged, err
	}
	if found.Status == http.StatusNotFound {
		return Created, c.writeDatasource(ctx, http.MethodPost, "/api/datasources", want, timeout)
	}
	if found.Status != http.StatusOK {
		return Unchanged, fmt.Errorf("reading datasource %s: %s",
			want.UID, answerText(found))
	}
	if sameDatasource(found.Body, want) {
		return Unchanged, nil
	}
	return Updated, c.writeDatasource(ctx, http.MethodPut,
		"/api/datasources/uid/"+want.UID, want, timeout)
}

// writeDatasource posts or puts the body both calls share.
func (c Client) writeDatasource(ctx context.Context, method, path string,
	want Datasource, timeout time.Duration,
) error {
	res, err := c.Do(ctx, method, path, datasourceBody(want), timeout)
	if err != nil {
		return err
	}
	if res.Status < 200 || res.Status > 299 {
		return fmt.Errorf("writing datasource %s: %s", want.UID, answerText(res))
	}
	return nil
}

// datasourceBody is the datasource in the shape the API takes. Access is
// always "proxy": the browser reaching a store directly is the other mode, and
// a store this collector writes to is not one a browser can be assumed to
// reach at all.
func datasourceBody(want Datasource) map[string]any {
	settings := map[string]any{}
	maps.Copy(settings, want.JSON)
	settings[fingerprintField] = fingerprint(want.Secret)
	body := map[string]any{
		"uid":      want.UID,
		"name":     want.Name,
		"type":     want.Type,
		"url":      want.URL,
		"access":   "proxy",
		"jsonData": settings,
	}
	if want.Database != "" {
		body["database"] = want.Database
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
	if field(have, "type") != want.Type || field(have, "url") != want.URL ||
		field(have, "name") != want.Name {
		return false
	}
	if want.Database != "" && field(have, "database") != want.Database {
		return false
	}
	settings, _ := have["jsonData"].(map[string]any)
	for key, value := range want.JSON {
		if fmt.Sprint(settings[key]) != fmt.Sprint(value) {
			return false
		}
	}
	return field(settings, fingerprintField) == fingerprint(want.Secret)
}

// fingerprint identifies a set of secrets without carrying them. The keys go
// in with the values so that moving a token from one field to another counts
// as a change.
func fingerprint(secret map[string]string) string {
	if len(secret) == 0 {
		return ""
	}
	sum := sha256.New()
	sum.Write([]byte(secretLabel))
	for _, key := range sortedKeys(secret) {
		sum.Write([]byte(key))
		sum.Write([]byte{0})
		sum.Write([]byte(secret[key]))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))[:12]
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
	res, err := c.Do(ctx, http.MethodGet, "/api/datasources/uid/"+uid+"/health", nil, timeout)
	if err != nil {
		return false, "", err
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

// sortedKeys keeps the fingerprint the same across runs, since map order is
// not.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}
