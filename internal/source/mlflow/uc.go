package mlflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/source"
)

// operationName is what a long-running operation is called ("operations/<id>");
// MLflow's own client puts it in the path as it is.
var operationName = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

var ucSchemaName = regexp.MustCompile(`^[A-Za-z0-9_]+\.[A-Za-z0-9_]+$`)

// databricks sets the connector up for a Databricks workspace. Locations are
// experiment IDs (read through the v3 API and signed download URLs) or Unity
// Catalog schemas "catalog.schema" (read through the v4 API, on a SQL
// warehouse); the two are not mixed in one source, as MLflow's own client
// refuses to. The credential is a bearer token, or with auth.databricks.client_id
// the secret of a service principal, exchanged for a token at the workspace's
// own /oidc/v1/token (databricks-sdk 0.151.0 oauth.py). The exchange goes to
// the endpoint's host only, so the credential grant for that host covers it.
func (c *connector) databricks(u *url.URL, token string) error {
	if token == "" {
		return fmt.Errorf("mlflow databricks: a workspace bearer token is required")
	}
	auth := c.src.GetAuth().GetDatabricks()
	if id := auth.GetClientId(); id != "" {
		// The secret goes in HTTP Basic authentication; the generic endpoint
		// guard allows plain HTTP to listed hosts, which a workspace never needs.
		if u.Scheme != "https" {
			return fmt.Errorf("mlflow databricks: auth.databricks.client_id sends the service principal's secret to the workspace, so endpoint must be https")
		}
		c.auth = &oauthClient{
			tokenURL: u.Scheme + "://" + u.Host + "/oidc/v1/token", clientID: id, secret: token,
			scope: "all-apis", client: c.client, now: time.Now,
		}
	}
	var experiments, schemas int
	for _, location := range c.src.GetLocations() {
		if _, err := strconv.ParseUint(location, 10, 64); err == nil {
			experiments++
		} else if ucSchemaName.MatchString(location) {
			schemas++
			c.ucSchemas = append(c.ucSchemas, location)
		} else {
			return fmt.Errorf("mlflow databricks: location %q must be a numeric experiment ID or a Unity Catalog schema, catalog.schema; experiment names and table prefixes are not built", location)
		}
	}
	switch {
	case experiments > 0 && schemas > 0:
		return fmt.Errorf("mlflow databricks: a source reads experiments or Unity Catalog schemas, not both; make two sources")
	case schemas > 0 && auth.GetSqlWarehouseId() == "":
		return fmt.Errorf("mlflow databricks: Unity Catalog schemas are searched on a SQL warehouse; set auth.databricks.sql_warehouse_id")
	case experiments > 0 && auth.GetSqlWarehouseId() != "":
		return fmt.Errorf("mlflow databricks: auth.databricks.sql_warehouse_id is for Unity Catalog schema locations, not experiments")
	}
	c.warehouse = auth.GetSqlWarehouseId()
	return nil
}

const ucPrefix = "/api/4.0/mlflow/traces"

// ucRef splits a trace's source ID, "catalog.schema/<trace id>": the v4 API
// addresses a trace by its location and its bare ID.
func ucRef(id string) (location, trace string, err error) {
	location, trace, ok := strings.Cut(id, "/")
	if !ok || !ucSchemaName.MatchString(location) || trace == "" || strings.Contains(trace, "/") {
		return "", "", fmt.Errorf("mlflow: %q is not a Unity Catalog trace ID (catalog.schema/<id>)", id)
	}
	return location, trace, nil
}

// operation is a long-running search (SearchTracesOperation in MLflow's
// databricks_tracing.proto).
type operation struct {
	Name  string `json:"name"`
	Done  bool   `json:"done"`
	Error *struct {
		Code string `json:"error_code"`
		Msg  string `json:"message"`
	} `json:"error"`
	Response struct {
		Traces []traceInfo `json:"trace_infos"`
		Next   string      `json:"next_page_token"`
	} `json:"response"`
}

// listUC searches the schemas for traces that started at or after req.Since,
// oldest first. The search runs as a long-running operation, polled until done.
func (c *connector) listUC(ctx context.Context, req source.ListRequest) (source.ListResult, error) {
	locations := make([]map[string]any, len(c.ucSchemas))
	for i, l := range c.ucSchemas {
		catalog, schema, _ := strings.Cut(l, ".")
		locations[i] = map[string]any{"type": "UC_SCHEMA", "uc_schema": map[string]any{"catalog_name": catalog, "schema_name": schema}}
	}
	body := map[string]any{
		"locations":        locations,
		"filter":           fmt.Sprintf("trace.timestamp_ms >= %d", req.Since.UnixMilli()),
		"order_by":         []string{"timestamp_ms ASC"},
		"max_results":      max(1, min(req.Limit, maxSearch)),
		"sql_warehouse_id": c.warehouse,
	}
	if req.Cursor != "" {
		body["page_token"] = req.Cursor
	}
	var op operation
	if err := c.call(ctx, http.MethodPost, ucPrefix+"/search-long-running", nil, body, &op); err != nil {
		return source.ListResult{}, err
	}
	// A search the warehouse cannot finish in this long is given up on; the
	// manager tries again next cycle.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for !op.Done {
		if !operationName.MatchString(op.Name) || strings.Contains(op.Name, "..") {
			return source.ListResult{}, fmt.Errorf("mlflow: the search operation has no usable name to poll (%q)", op.Name)
		}
		select {
		case <-ctx.Done():
			return source.ListResult{}, fmt.Errorf("mlflow: the Unity Catalog search did not finish: %w", ctx.Err())
		case <-time.After(c.pollEvery):
		}
		next := operation{}
		if err := c.call(ctx, http.MethodGet, ucPrefix+"/search/operations/"+op.Name, nil, nil, &next); err != nil {
			return source.ListResult{}, err
		}
		if next.Name == "" {
			next.Name = op.Name
		}
		op = next
	}
	if op.Error != nil {
		return source.ListResult{}, fmt.Errorf("mlflow: the Unity Catalog search failed: %s %s", op.Error.Code, op.Error.Msg)
	}
	out := source.ListResult{Next: op.Response.Next}
	for _, t := range op.Response.Traces {
		started, err := time.Parse(time.RFC3339Nano, t.RequestTime)
		if err != nil {
			return source.ListResult{}, fmt.Errorf("mlflow: trace %s has request_time %q: %w", t.TraceID, t.RequestTime, err)
		}
		id, err := ucID(t)
		if err != nil {
			return source.ListResult{}, err
		}
		out.Infos = append(out.Infos, source.Info{ID: id, Started: started.UTC(), InProgress: t.State == "IN_PROGRESS", Digest: digest(t)})
	}
	return out, nil
}

// ucID is the source's ID for a trace: its schema and its own ID.
func ucID(t traceInfo) (string, error) {
	l := t.Location.UCSchema
	if l.Catalog == "" || l.Schema == "" || t.TraceID == "" {
		return "", fmt.Errorf("mlflow: trace %q came back without a Unity Catalog schema location", t.TraceID)
	}
	return l.Catalog + "." + l.Schema + "/" + t.TraceID, nil
}

// fetchUC reads the spans of the listed traces, one request per schema.
func (c *connector) fetchUC(ctx context.Context, infos []source.Info) ([]ingest.Trace, error) {
	bySchema := map[string][]string{}
	var order []string
	for _, in := range infos {
		location, id, err := ucRef(in.ID)
		if err != nil {
			return nil, err
		}
		if _, ok := bySchema[location]; !ok {
			order = append(order, location)
		}
		bySchema[location] = append(bySchema[location], id)
	}
	var out []ingest.Trace
	const batch = 50
	for _, location := range order {
		ids := bySchema[location]
		for start := 0; start < len(ids); start += batch {
			traces, err := c.batchGetUC(ctx, location, ids[start:min(start+batch, len(ids))])
			if err != nil {
				return nil, err
			}
			out = append(out, traces...)
		}
	}
	return out, nil
}

type ucTrace struct {
	Info  traceInfo         `json:"trace_info"`
	Spans []json.RawMessage `json:"spans"`
}

func (c *connector) batchGetUC(ctx context.Context, location string, ids []string) ([]ingest.Trace, error) {
	q := url.Values{"sql_warehouse_id": {c.warehouse}}
	for _, id := range ids {
		q.Add("trace_ids", id)
	}
	var resp struct {
		Traces []ucTrace `json:"traces"`
	}
	if err := c.call(ctx, http.MethodGet, ucPrefix+"/"+url.PathEscape(location)+"/batchGet", q, nil, &resp); err != nil {
		return nil, err
	}
	var out []ingest.Trace
	for _, t := range resp.Traces {
		id, err := ucID(t.Info)
		if err != nil {
			return nil, err
		}
		// The trace's source ID is the one the write-back addresses it by.
		t.Info.TraceID = id
		tr, err := toTrace(t.Info, t.Spans)
		if err != nil {
			return nil, err
		}
		if len(tr.Spans) > 0 {
			out = append(out, tr)
		}
	}
	return out, nil
}

// assessmentsUC reads the assessments already on a trace.
func (c *connector) assessmentsUC(ctx context.Context, id string) ([]assessment, error) {
	location, trace, err := ucRef(id)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Traces []ucTrace `json:"traces"`
	}
	q := url.Values{"sql_warehouse_id": {c.warehouse}, "trace_ids": {trace}}
	err = c.call(ctx, http.MethodGet, ucPrefix+"/"+url.PathEscape(location)+"/batchGet", q, nil, &resp)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil || len(resp.Traces) == 0 {
		return nil, err
	}
	return resp.Traces[0].Info.Assessments, nil
}

func ucLocation(location string) map[string]any {
	catalog, schema, _ := strings.Cut(location, ".")
	return map[string]any{"type": "UC_SCHEMA", "uc_schema": map[string]any{"catalog_name": catalog, "schema_name": schema}}
}

// createAssessmentUC adds a feedback assessment on a Unity Catalog trace.
func (c *connector) createAssessmentUC(ctx context.Context, id string, a map[string]any) (string, error) {
	location, trace, err := ucRef(id)
	if err != nil {
		return "", err
	}
	a["trace_id"] = trace
	a["trace_location"] = ucLocation(location)
	var created assessment
	err = c.call(ctx, http.MethodPost, ucPrefix+"/"+url.PathEscape(location)+"/"+url.PathEscape(trace)+"/assessments",
		url.Values{"sql_warehouse_id": {c.warehouse}}, a, &created)
	return created.ID, err
}

// updateAssessmentUC changes the value and rationale of an assessment.
func (c *connector) updateAssessmentUC(ctx context.Context, s source.Score, remote string) error {
	location, trace, err := ucRef(s.TraceID)
	if err != nil {
		return err
	}
	a := map[string]any{
		"assessment_id": remote, "trace_id": trace, "trace_location": ucLocation(location),
		"assessment_name": s.Metric, "feedback": map[string]any{"value": s.Value}, "rationale": s.Rationale,
	}
	return c.call(ctx, http.MethodPatch, ucPrefix+"/"+url.PathEscape(location)+"/"+url.PathEscape(trace)+"/assessments/"+url.PathEscape(remote),
		url.Values{"sql_warehouse_id": {c.warehouse}, "update_mask": {"feedback,rationale"}}, a, nil)
}
