package mlflow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// authorizer puts a credential on a request to the store. body is the
// request's body, which a signature covers.
type authorizer interface {
	Authorize(ctx context.Context, req *http.Request, body []byte) error
	// Rejected is called when the store refused the credential on sent (HTTP
	// 401), so the next Authorize obtains a fresh one. It reports whether a
	// retry can help.
	Rejected(sent *http.Request) bool
}

// bearer sends a fixed token: an open-source server's token, or a Databricks
// personal access token. A refused token stays refused.
type bearer string

func (b bearer) Authorize(_ context.Context, req *http.Request, _ []byte) error {
	if b != "" {
		req.Header.Set("Authorization", "Bearer "+string(b))
	}
	return nil
}

func (bearer) Rejected(*http.Request) bool { return false }

// oauthClient exchanges a service principal's client ID and secret for an
// access token (the OAuth client-credentials grant) and keeps it until just
// before it expires. The token endpoint is fixed by the caller from the
// source's endpoint, never from the store's responses, so the secret goes only
// where the credential grant already allows.
//
// The manager builds a source's connector afresh each cycle, so tokens are kept
// in a process-wide cache, keyed by endpoint, client ID and a hash of the secret:
// a rotated secret is a new key and never reuses the old token.
type oauthClient struct {
	tokenURL string
	clientID string
	secret   string
	scope    string
	client   *http.Client
	now      func() time.Time
}

type cachedToken struct {
	mu      sync.Mutex // held while the token is exchanged, so concurrent callers share one exchange
	token   string
	expires time.Time
}

var (
	tokensMu sync.Mutex
	tokens   = map[string]*cachedToken{}
)

// refreshSkew is how long before expiry a cached token is replaced.
const refreshSkew = time.Minute

func (o *oauthClient) entry() (string, *cachedToken) {
	key := o.tokenURL + "\x00" + o.clientID + "\x00" + o.scope + "\x00" + sha256Hex([]byte(o.secret))
	tokensMu.Lock()
	defer tokensMu.Unlock()
	e := tokens[key]
	if e == nil {
		// Forget what has expired, so rotated secrets do not accumulate.
		for k, old := range tokens {
			if old.mu.TryLock() {
				if old.token == "" || !o.now().Before(old.expires) {
					delete(tokens, k)
				}
				old.mu.Unlock()
			}
		}
		e = &cachedToken{}
		tokens[key] = e
	}
	return key, e
}

func (o *oauthClient) Authorize(ctx context.Context, req *http.Request, _ []byte) error {
	token, err := o.get(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// Rejected drops the cached token the store refused: it was revoked, or the
// clock skewed. A token another runner has already replaced is kept, so a late
// 401 for the old one does not cost a new exchange.
func (o *oauthClient) Rejected(sent *http.Request) bool {
	refused := strings.TrimPrefix(sent.Header.Get("Authorization"), "Bearer ")
	_, e := o.entry()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.token == refused {
		e.token = ""
	}
	return true
}

func (o *oauthClient) get(ctx context.Context) (string, error) {
	_, e := o.entry()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.token != "" && o.now().Add(refreshSkew).Before(e.expires) {
		return e.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {o.scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(o.clientID), url.QueryEscape(o.secret))
	// A token endpoint does not redirect; following one would send the secret on.
	client := *o.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mlflow: token request: %w", urlError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string `json:"access_token"`
		ExpiresIn int64  `json:"expires_in"`
		Error     string `json:"error"`
		Desc      string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		msg := strings.TrimSpace(out.Error + " " + out.Desc)
		if msg == "" {
			msg = strings.TrimSpace(string(raw[:min(len(raw), 200)]))
		}
		return "", fmt.Errorf("mlflow: token endpoint answered HTTP %d: %s", resp.StatusCode, msg)
	}
	e.token = out.Token
	e.expires = o.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return e.token, nil
}

// urlError drops the URL from a transport error, so a message does not repeat
// a credential-bearing address.
func urlError(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

// awsCredentials is the source credential for a SageMaker store: a JSON
// object, since the credential is a single secret value.
type awsCredentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

func parseAWSCredentials(s string) (awsCredentials, error) {
	var c awsCredentials
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	// One object and nothing after it: a second one would be signed with
	// silently ignored.
	if err := dec.Decode(&c); err != nil || dec.More() || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return awsCredentials{}, fmt.Errorf("mlflow sagemaker: the credential must be a JSON object with access_key_id and secret_access_key (and session_token for temporary credentials)")
	}
	return c, nil
}

// sigV4 signs requests with AWS Signature Version 4 (the "AWS4-HMAC-SHA256"
// algorithm), with the payload hash in X-Amz-Content-SHA256 as the SageMaker
// MLflow plugin sends it. Credentials come from the source's credential only:
// never from the server's own AWS identity, which a project editor could
// otherwise borrow.
type sigV4 struct {
	creds   awsCredentials
	region  string
	service string
	now     func() time.Time
}

func (s *sigV4) Rejected(*http.Request) bool { return false }

func (s *sigV4) Authorize(_ context.Context, req *http.Request, body []byte) error {
	t := s.now().UTC()
	date := t.Format("20060102")
	stamp := t.Format("20060102T150405Z")
	payload := sha256Hex(body)
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-SHA256", payload)
	if s.creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.creds.SessionToken)
	}
	// Sign the host, the content type and the x-amz-*, x-mlflow-* and x-sm-*
	// routing headers: botocore, which the SageMaker plugin signs with, signs
	// every header it is given.
	headers := map[string]string{"host": req.URL.Host}
	for k := range req.Header {
		if lk := strings.ToLower(k); lk == "content-type" || strings.HasPrefix(lk, "x-amz-") || strings.HasPrefix(lk, "x-mlflow-") || strings.HasPrefix(lk, "x-sm-") {
			headers[lk] = strings.Join(strings.Fields(req.Header.Get(k)), " ")
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, k := range names {
		canonical.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	request := strings.Join([]string{
		req.Method, canonicalPath(req.URL), canonicalQuery(req.URL.Query()), canonical.String(), signed, payload,
	}, "\n")
	scope := date + "/" + s.region + "/" + s.service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + sha256Hex([]byte(request))
	key := hmacSHA256([]byte("AWS4"+s.creds.SecretAccessKey), date)
	for _, part := range []string{s.region, s.service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.creds.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
	return nil
}

// canonicalPath is the URI-encoded path. Every service but S3 encodes it
// twice; the path here has no characters that need it once.
func canonicalPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if un, err := url.PathUnescape(s); err == nil {
			s = un
		}
		segs[i] = awsEscape(awsEscape(s))
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// awsEscape percent-encodes everything but the unreserved characters.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
