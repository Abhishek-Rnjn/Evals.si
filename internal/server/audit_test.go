package server

import (
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

// breakAudit makes every audit write fail, as an audit store outage would.
func breakAudit(t *testing.T, dataDir string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "evalsi.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE audit`); err != nil {
		t.Fatal(err)
	}
}

func projectNamesOf(t *testing.T, c evalsiv1alpha1connect.AuthServiceClient) string {
	t.Helper()
	resp, err := c.ListProjects(t.Context(), connect.NewRequest(&evalsiv1alpha1.ListProjectsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range resp.Msg.GetProjects() {
		names = append(names, p.GetName())
	}
	return strings.Join(names, ",")
}

// Decision 0026, strict mode: with the audit store down, a change is refused
// (Unavailable) and not made; a denial is still a denial. In best-effort mode
// the change is made and the failure is counted.
func TestStrictAuditRefusesAChangeItCannotRecord(t *testing.T) {
	for _, mode := range []string{"", "besteffort"} {
		t.Run("mode="+mode, func(t *testing.T) {
			var dataDir string
			s := startAuthServer(t, func(c *config.Config) {
				c.Audit.Mode = mode
				dataDir = c.DataDir
			})
			owner := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["owner"]))
			breakAudit(t, dataDir)

			_, err := owner.CreateProject(t.Context(), connect.NewRequest(&evalsiv1alpha1.CreateProjectRequest{Name: "labs"}))
			made := strings.Contains(","+projectNamesOf(t, owner)+",", ",labs,")
			switch mode {
			case "": // strict, the default with authentication on
				if connect.CodeOf(err) != connect.CodeUnavailable || made {
					t.Fatalf("strict: err %v, project made %v", err, made)
				}
			default:
				if err != nil || !made {
					t.Fatalf("besteffort: err %v, project made %v", err, made)
				}
			}

			admin := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["admin"]))
			if _, err := admin.CreateProject(t.Context(), connect.NewRequest(&evalsiv1alpha1.CreateProjectRequest{Name: "x"})); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("a denial must stay a denial: %v", err)
			}

			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, s.url+"/metrics", nil)
			req.Header.Set("Authorization", "Bearer "+s.keys["owner"])
			resp, err := s.http.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if strings.Contains(string(body), "evalsi_audit_write_failures_total 0\n") || !strings.Contains(string(body), "evalsi_audit_write_failures_total") {
				t.Errorf("failed audit writes not counted:\n%s", body)
			}
		})
	}
}

func auditEvents(t *testing.T, s *testServer, action string) []*evalsiv1alpha1.AuditEvent {
	t.Helper()
	owner := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["owner"]))
	resp, err := owner.ListAuditEvents(t.Context(), connect.NewRequest(&evalsiv1alpha1.ListAuditEventsRequest{Action: action, PageSize: 1000}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetEvents()
}

// Decision 0026: entries say how the caller authenticated (never the
// credential), reads of the configured kinds are recorded, the configuration
// in effect is recorded, and the hash chain verifies.
func TestAuditEntriesCarryAuthContextReadsConfigAndAChain(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) { c.Audit.Reads = []string{"runs"} })
	ctx := t.Context()
	owner := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["owner"]))
	if _, err := owner.CreateProject(ctx, connect.NewRequest(&evalsiv1alpha1.CreateProjectRequest{Name: "labs"})); err != nil {
		t.Fatal(err)
	}
	var keyed *evalsiv1alpha1.AuditEvent
	for _, ev := range auditEvents(t, s, "projects.manage") {
		if ev.GetPrincipal() == "key:owner" {
			keyed = ev
		}
	}
	if keyed == nil || keyed.GetAuthMethod() != "apikey" || keyed.GetApiKey() != "owner" ||
		keyed.GetSubject() != "key:owner" || keyed.GetActor() != "key:owner" || keyed.GetSequence() == 0 || keyed.GetHash() == "" {
		t.Fatalf("api-key entry = %v", keyed)
	}

	// A JWT caller's denial records its issuer, token ID and key ID.
	tok := s.token(t, map[string]any{"sub": "repo:acme/agent", "repository": "acme/agent", "ref": "refs/heads/main", "jti": "tok-7"})
	jwtAuth := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(tok))
	if _, err := jwtAuth.CreateProject(ctx, connect.NewRequest(&evalsiv1alpha1.CreateProjectRequest{Name: "y"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("jwt create project: %v", err)
	}
	var jwtEntry *evalsiv1alpha1.AuditEvent
	for _, ev := range auditEvents(t, s, "projects.manage") {
		if ev.GetAuthMethod() == "jwt" {
			jwtEntry = ev
		}
	}
	if jwtEntry == nil || jwtEntry.GetIssuer() != "https://token.actions.githubusercontent.com" ||
		jwtEntry.GetTokenId() != "tok-7" || jwtEntry.GetKeyId() != "k1" || jwtEntry.GetAllowed() {
		t.Fatalf("jwt entry = %v", jwtEntry)
	}
	for _, ev := range auditEvents(t, s, "") {
		if strings.Contains(ev.String(), s.keys["owner"]) || strings.Contains(ev.String(), tok) {
			t.Fatalf("a credential reached the audit log: %v", ev)
		}
	}

	// audit.reads: [runs] records a run listing, not other reads.
	viewer := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["viewer"]))
	if _, err := viewer.ListRuns(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunsRequest{})); err != nil {
		t.Fatal(err)
	}
	reads := auditEvents(t, s, "runs.read")
	if len(reads) == 0 || reads[0].GetPrincipal() != "key:viewer" || !reads[0].GetAllowed() {
		t.Fatalf("run read not recorded: %v", reads)
	}
	if got := auditEvents(t, s, "audit.read"); len(got) != 0 {
		t.Fatalf("an unconfigured read kind was recorded: %v", got)
	}

	if loaded := auditEvents(t, s, "config.load"); len(loaded) != 1 || !strings.Contains(loaded[0].GetDetail(), `"credentials":"`) {
		t.Fatalf("config.load = %v", loaded)
	}

	res, err := owner.VerifyAuditLog(ctx, connect.NewRequest(&evalsiv1alpha1.VerifyAuditLogRequest{}))
	if err != nil || !res.Msg.GetOk() || res.Msg.GetChecked() < 4 || res.Msg.GetFirstSequence() != 1 {
		t.Fatalf("verify = %v, %v", res, err)
	}
	admin := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["admin"]))
	if _, err := admin.VerifyAuditLog(ctx, connect.NewRequest(&evalsiv1alpha1.VerifyAuditLogRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("a project admin verified the whole log: %v", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.url+"/v1alpha1/audit:verify", nil)
	req.Header.Set("Authorization", "Bearer "+s.keys["owner"])
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("REST verify: %d %s", resp.StatusCode, body)
	}
}
