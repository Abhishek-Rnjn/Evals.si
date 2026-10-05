package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

// ServiceStore is what AuthService reads and writes.
type ServiceStore interface {
	Store
	CreateProject(ctx context.Context, p Project) error
	PutRole(ctx context.Context, r Role, create bool) error
	DeleteRole(ctx context.Context, project, name string) error
	CreateBinding(ctx context.Context, b Binding) error
	DeleteBinding(ctx context.Context, b Binding) error
	CreateAPIKey(ctx context.Context, k KeyRecord) error
	APIKeys(ctx context.Context) ([]KeyRecord, error)
	APIKey(ctx context.Context, name string) (KeyRecord, error)
	RevokeAPIKey(ctx context.Context, name string) error
	ListAudit(ctx context.Context, q AuditQuery, pageSize int, pageToken string) ([]*evalsiv1alpha1.AuditEvent, string, error)
}

// Service implements AuthService. The enforcement point has already checked
// the action named in the action table for each call; the service adds what
// needs the request's content: privilege-escalation checks, validation, and
// filtering of list results.
type Service struct {
	engine     *Engine
	store      ServiceStore
	auditor    *Auditor
	configKeys []auth.ConfigKey
	now        func() time.Time
}

// NewService builds the AuthService.
func NewService(e *Engine, st ServiceStore, auditor *Auditor, configKeys []auth.ConfigKey) *Service {
	return &Service{engine: e, store: st, auditor: auditor, configKeys: configKeys, now: time.Now}
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func storeErr(err error, what string) error {
	switch {
	case errors.Is(err, ErrExists):
		return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already exists", what))
	case errors.Is(err, ErrNotFound):
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("no %s", what))
	}
	return err
}

func denied(err error) error { return connect.NewError(connect.CodePermissionDenied, err) }

// record writes a detailed audit event for a change made through the API.
func (s *Service) record(ctx context.Context, action, project, resource string, detail any) {
	p := auth.PrincipalFrom(ctx)
	ev := &evalsiv1alpha1.AuditEvent{
		Principal: p.ID(), Action: action, Project: project, Resource: resource, Allowed: true,
	}
	if c := CheckerFrom(ctx); c != nil {
		ev.Reason, ev.Procedure, ev.Source = c.Decision.Reason, c.Meta.Procedure, c.Meta.Source
		ev.RequestId = c.Meta.Headers.Get("X-Request-Id")
	}
	if detail != nil {
		raw, _ := json.Marshal(detail)
		ev.Detail = string(raw)
	}
	s.auditor.Record(ctx, ev)
}

// WhoAmI implements AuthService.
func (s *Service) WhoAmI(ctx context.Context, _ *connect.Request[evalsiv1alpha1.WhoAmIRequest]) (*connect.Response[evalsiv1alpha1.WhoAmIResponse], error) {
	p := auth.PrincipalFrom(ctx)
	owner, access := s.engine.Access(p)
	resp := &evalsiv1alpha1.WhoAmIResponse{
		Principal: &evalsiv1alpha1.Principal{
			Kind: p.Kind, Provider: p.Provider, Subject: p.Subject, Name: p.Name, Email: p.Email,
			Groups: p.Groups, Method: p.Method, Id: p.ID(), Labels: p.Labels,
		},
		Owner:       owner,
		AuthEnabled: s.engine.Enabled(),
	}
	for _, a := range access {
		resp.Access = append(resp.Access, &evalsiv1alpha1.ProjectAccess{Project: a.Project, Roles: a.Roles, Permissions: a.Permissions})
	}
	return connect.NewResponse(resp), nil
}

// visibleProjects is the set of projects a principal holds any role in; nil means all.
func (s *Service) visibleProjects(p *auth.Principal) map[string]bool {
	owner, access := s.engine.Access(p)
	if owner || !s.engine.Enabled() {
		return nil
	}
	out := map[string]bool{}
	for _, a := range access {
		if a.Project == "*" {
			return nil
		}
		out[a.Project] = true
	}
	return out
}

func projectProto(p Project) *evalsiv1alpha1.Project {
	out := &evalsiv1alpha1.Project{Name: p.Name, Description: p.Description, Source: p.Source, CreatedBy: p.CreatedBy}
	if !p.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(p.CreatedAt)
	}
	return out
}

// ListProjects implements AuthService.
func (s *Service) ListProjects(ctx context.Context, _ *connect.Request[evalsiv1alpha1.ListProjectsRequest]) (*connect.Response[evalsiv1alpha1.ListProjectsResponse], error) {
	visible := s.visibleProjects(auth.PrincipalFrom(ctx))
	resp := &evalsiv1alpha1.ListProjectsResponse{}
	for _, p := range s.engine.Projects() {
		if visible == nil || visible[p.Name] {
			resp.Projects = append(resp.Projects, projectProto(p))
		}
	}
	return connect.NewResponse(resp), nil
}

// CreateProject implements AuthService.
func (s *Service) CreateProject(ctx context.Context, req *connect.Request[evalsiv1alpha1.CreateProjectRequest]) (*connect.Response[evalsiv1alpha1.CreateProjectResponse], error) {
	name := req.Msg.GetName()
	if !ValidProjectName(name) {
		return nil, invalid("invalid project name %q: use lower-case letters, digits and dashes", name)
	}
	if s.engine.ProjectExists(name) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("project %q already exists", name))
	}
	p := Project{Name: name, Description: req.Msg.GetDescription(), CreatedAt: s.now().UTC(), CreatedBy: auth.PrincipalFrom(ctx).ID(), Source: SourceAPI}
	if err := s.store.CreateProject(ctx, p); err != nil {
		return nil, storeErr(err, "project "+name)
	}
	if err := s.engine.Reload(ctx); err != nil {
		return nil, err
	}
	s.record(ctx, "projects.manage", name, "project/"+name, map[string]any{"new": p})
	return connect.NewResponse(&evalsiv1alpha1.CreateProjectResponse{Project: projectProto(p)}), nil
}

// --- API keys ---

func rolesFromProto(m map[string]*evalsiv1alpha1.RoleList) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		out[k] = slices.Clone(v.GetRoles())
	}
	return out
}

func rolesToProto(m map[string][]string) map[string]*evalsiv1alpha1.RoleList {
	out := map[string]*evalsiv1alpha1.RoleList{}
	for k, v := range m {
		out[k] = &evalsiv1alpha1.RoleList{Roles: v}
	}
	return out
}

func keyProto(k KeyRecord, source string) *evalsiv1alpha1.APIKey {
	out := &evalsiv1alpha1.APIKey{
		Name: k.Name, Roles: rolesToProto(k.Roles), Labels: k.Labels, CreatedBy: k.CreatedBy,
		Source: source, Prefix: k.Prefix, Revoked: k.Revoked,
	}
	for _, t := range []struct {
		at  time.Time
		dst **timestamppb.Timestamp
	}{{k.CreatedAt, &out.CreatedAt}, {k.ExpiresAt, &out.ExpiresAt}, {k.LastUsedAt, &out.LastUsedAt}} {
		if !t.at.IsZero() {
			*t.dst = timestamppb.New(t.at)
		}
	}
	return out
}

// checkKeyRoles validates a key's roles and that the caller may grant them.
func (s *Service) checkKeyRoles(p *auth.Principal, roles map[string][]string) error {
	if len(roles) == 0 {
		return invalid("an API key needs roles in at least one project")
	}
	for project, names := range roles {
		if project != "*" && !s.engine.ProjectExists(project) {
			return invalid("unknown project %q", project)
		}
		for _, r := range names {
			if !s.engine.ResolveRole(project, r) {
				return invalid("unknown role %q in project %q", r, project)
			}
		}
		if err := s.engine.CheckRoleNames(p, project, names); err != nil {
			return denied(err)
		}
	}
	return nil
}

func validLabels(labels map[string]string) error {
	for k, v := range labels {
		if !ValidRoleName(k) || len(v) > 256 {
			return invalid("label %q: keys are lower-case names, values at most 256 characters", k)
		}
	}
	return nil
}

// CreateAPIKey implements AuthService.
func (s *Service) CreateAPIKey(ctx context.Context, req *connect.Request[evalsiv1alpha1.CreateAPIKeyRequest]) (*connect.Response[evalsiv1alpha1.CreateAPIKeyResponse], error) {
	p := auth.PrincipalFrom(ctx)
	name := req.Msg.GetName()
	if !ValidRoleName(name) {
		return nil, invalid("invalid key name %q", name)
	}
	for _, k := range s.configKeys {
		if k.Name == name {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("API key %q is declared in config", name))
		}
	}
	roles := rolesFromProto(req.Msg.GetRoles())
	if err := s.checkKeyRoles(p, roles); err != nil {
		return nil, err
	}
	if err := validLabels(req.Msg.GetLabels()); err != nil {
		return nil, err
	}
	secret, hash := auth.NewAPIKey()
	rec := KeyRecord{
		Name: name, Hash: hash, Prefix: secret[:len(auth.APIKeyPrefix)+6], Roles: roles, Labels: req.Msg.GetLabels(),
		CreatedAt: s.now().UTC(), CreatedBy: p.ID(),
	}
	if ttl := req.Msg.GetTtl(); ttl != nil {
		if ttl.AsDuration() <= 0 {
			return nil, invalid("ttl must be positive")
		}
		rec.ExpiresAt = rec.CreatedAt.Add(ttl.AsDuration())
	}
	if err := s.store.CreateAPIKey(ctx, rec); err != nil {
		return nil, storeErr(err, "API key "+name)
	}
	s.record(ctx, "access.manage", keyProject(roles), "apikey/"+name, map[string]any{"roles": roles, "labels": rec.Labels, "expires_at": rec.ExpiresAt})
	return connect.NewResponse(&evalsiv1alpha1.CreateAPIKeyResponse{Key: keyProto(rec, SourceAPI), Secret: secret}), nil
}

// keyProject is the project an API key event is filed under: its only project, or "*".
func keyProject(roles map[string][]string) string {
	if len(roles) == 1 {
		for p := range roles {
			return p
		}
	}
	return "*"
}

// canManageKey reports whether the caller may see or revoke a key: it must
// hold access.manage in every project the key has roles in.
func (s *Service) canManageKey(ctx context.Context, roles map[string][]string) bool {
	for project := range roles {
		if project == "*" {
			if !s.engine.IsOwner(auth.PrincipalFrom(ctx)) {
				return false
			}
			continue
		}
		if !Can(ctx, "access.manage", project, nil) {
			return false
		}
	}
	return true
}

// ListAPIKeys implements AuthService.
func (s *Service) ListAPIKeys(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListAPIKeysRequest]) (*connect.Response[evalsiv1alpha1.ListAPIKeysResponse], error) {
	keys := make([]*evalsiv1alpha1.APIKey, 0)
	add := func(k KeyRecord, source string) {
		if project := req.Msg.GetProject(); project != "" {
			if _, ok := k.Roles[project]; !ok {
				return
			}
		}
		if s.canManageKey(ctx, k.Roles) {
			keys = append(keys, keyProto(k, source))
		}
	}
	for _, ck := range s.configKeys {
		rec := KeyRecord{Name: ck.Name, Roles: ck.Roles, Labels: ck.Labels}
		if ck.ExpiresAt != "" {
			rec.ExpiresAt, _ = time.Parse(time.RFC3339, ck.ExpiresAt)
		}
		add(rec, SourceConfig)
	}
	stored, err := s.store.APIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range stored {
		add(k, SourceAPI)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].GetName() < keys[j].GetName() })
	return connect.NewResponse(&evalsiv1alpha1.ListAPIKeysResponse{Keys: keys}), nil
}

// KeyRoles returns the roles of a key, for the enforcement point to check
// access.manage in each of its projects before RevokeAPIKey.
func (s *Service) KeyRoles(ctx context.Context, name string) (map[string][]string, bool, error) {
	for _, k := range s.configKeys {
		if k.Name == name {
			return k.Roles, true, nil
		}
	}
	k, err := s.store.APIKey(ctx, name)
	if err != nil {
		return nil, false, err
	}
	return k.Roles, false, nil
}

// RevokeAPIKey implements AuthService.
func (s *Service) RevokeAPIKey(ctx context.Context, req *connect.Request[evalsiv1alpha1.RevokeAPIKeyRequest]) (*connect.Response[evalsiv1alpha1.RevokeAPIKeyResponse], error) {
	name := req.Msg.GetName()
	roles, fromConfig, err := s.KeyRoles(ctx, name)
	if err != nil {
		return nil, storeErr(err, "API key "+name)
	}
	if fromConfig {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("API key %q is declared in config; remove it there", name))
	}
	if err := s.store.RevokeAPIKey(ctx, name); err != nil {
		return nil, storeErr(err, "API key "+name)
	}
	s.record(ctx, "access.manage", keyProject(roles), "apikey/"+name, map[string]any{"revoked": true})
	return connect.NewResponse(&evalsiv1alpha1.RevokeAPIKeyResponse{}), nil
}

// --- roles ---

func roleProto(r Role) *evalsiv1alpha1.Role {
	return &evalsiv1alpha1.Role{
		Name: r.Name, Project: r.Project, Description: r.Description, Permissions: r.Permissions,
		Inherits: r.Inherits, Condition: r.Condition, Source: r.Source,
	}
}

func roleFromProto(r *evalsiv1alpha1.Role) Role {
	return Role{
		Name: r.GetName(), Project: r.GetProject(), Description: r.GetDescription(),
		Permissions: r.GetPermissions(), Inherits: r.GetInherits(), Condition: strings.TrimSpace(r.GetCondition()),
	}
}

// ListRoles implements AuthService.
func (s *Service) ListRoles(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListRolesRequest]) (*connect.Response[evalsiv1alpha1.ListRolesResponse], error) {
	visible := s.visibleProjects(auth.PrincipalFrom(ctx))
	want := req.Msg.GetProject()
	resp := &evalsiv1alpha1.ListRolesResponse{}
	for _, r := range s.engine.Roles() {
		if r.Project != "" && ((want != "" && r.Project != want) || (visible != nil && !visible[r.Project])) {
			continue
		}
		resp.Roles = append(resp.Roles, roleProto(r))
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) putRole(ctx context.Context, msg *evalsiv1alpha1.Role, create bool) (*evalsiv1alpha1.Role, error) {
	if msg == nil {
		return nil, invalid("role is required")
	}
	def := roleFromProto(msg)
	old, exists := s.engine.Role(def.Project, def.Name)
	switch {
	case create && exists:
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("role %q already exists", def.Name))
	case !create && !exists:
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no role %q in project %q", def.Name, def.Project))
	case exists && old.Source != SourceAPI:
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("role %q is %s; change it there", def.Name, old.Source))
	}
	if err := s.engine.ValidateRole(def, !create); err != nil {
		return nil, invalid("role %q: %v", def.Name, err)
	}
	if err := s.engine.CheckRole(auth.PrincipalFrom(ctx), or(def.Project, "*"), def); err != nil {
		return nil, denied(err)
	}
	if err := s.store.PutRole(ctx, def, create); err != nil {
		return nil, storeErr(err, "role "+def.Name)
	}
	if err := s.engine.Reload(ctx); err != nil {
		return nil, err
	}
	detail := map[string]any{"new": def}
	if !create {
		detail["old"] = old
	}
	s.record(ctx, "access.manage", or(def.Project, "*"), "role/"+or(def.Project, "*")+"/"+def.Name, detail)
	def.Source = SourceAPI
	return roleProto(def), nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// CreateRole implements AuthService.
func (s *Service) CreateRole(ctx context.Context, req *connect.Request[evalsiv1alpha1.CreateRoleRequest]) (*connect.Response[evalsiv1alpha1.CreateRoleResponse], error) {
	r, err := s.putRole(ctx, req.Msg.GetRole(), true)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.CreateRoleResponse{Role: r}), nil
}

// UpdateRole implements AuthService.
func (s *Service) UpdateRole(ctx context.Context, req *connect.Request[evalsiv1alpha1.UpdateRoleRequest]) (*connect.Response[evalsiv1alpha1.UpdateRoleResponse], error) {
	r, err := s.putRole(ctx, req.Msg.GetRole(), false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.UpdateRoleResponse{Role: r}), nil
}

// DeleteRole implements AuthService.
func (s *Service) DeleteRole(ctx context.Context, req *connect.Request[evalsiv1alpha1.DeleteRoleRequest]) (*connect.Response[evalsiv1alpha1.DeleteRoleResponse], error) {
	project, name := req.Msg.GetProject(), req.Msg.GetName()
	old, ok := s.engine.Role(project, name)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no role %q in project %q", name, project))
	}
	if old.Source != SourceAPI {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("role %q is %s; remove it there", name, old.Source))
	}
	if users := s.engine.RoleUsers(project, name); len(users) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("role %q is in use by %s", name, strings.Join(users, ", ")))
	}
	if !s.engine.IsOwner(auth.PrincipalFrom(ctx)) && project == "" {
		return nil, denied(errors.New("only an owner can delete install-wide roles"))
	}
	if err := s.store.DeleteRole(ctx, project, name); err != nil {
		return nil, storeErr(err, "role "+name)
	}
	if err := s.engine.Reload(ctx); err != nil {
		return nil, err
	}
	s.record(ctx, "access.manage", or(project, "*"), "role/"+or(project, "*")+"/"+name, map[string]any{"old": old})
	return connect.NewResponse(&evalsiv1alpha1.DeleteRoleResponse{}), nil
}

// ListPermissions implements AuthService.
func (s *Service) ListPermissions(context.Context, *connect.Request[evalsiv1alpha1.ListPermissionsRequest]) (*connect.Response[evalsiv1alpha1.ListPermissionsResponse], error) {
	resp := &evalsiv1alpha1.ListPermissionsResponse{}
	for _, p := range Permissions {
		resp.Permissions = append(resp.Permissions, &evalsiv1alpha1.Permission{Name: p.Name, Description: p.Description, InstallWide: p.InstallWide})
	}
	return connect.NewResponse(resp), nil
}

// --- bindings ---

func bindingProto(b Binding) *evalsiv1alpha1.RoleBinding {
	out := &evalsiv1alpha1.RoleBinding{Project: b.Project, Role: b.Role, Subject: b.Subject, Source: b.Source, CreatedBy: b.CreatedBy}
	if !b.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(b.CreatedAt)
	}
	return out
}

// canManage reports whether the caller holds access.manage in a project ("*": owners only).
func (s *Service) canManage(ctx context.Context, project string) bool {
	if project == "*" {
		return s.engine.IsOwner(auth.PrincipalFrom(ctx))
	}
	return Can(ctx, "access.manage", project, nil)
}

// ListBindings implements AuthService.
func (s *Service) ListBindings(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListBindingsRequest]) (*connect.Response[evalsiv1alpha1.ListBindingsResponse], error) {
	resp := &evalsiv1alpha1.ListBindingsResponse{}
	allowed := map[string]bool{}
	for _, b := range s.engine.Bindings() {
		if want := req.Msg.GetProject(); want != "" && b.Project != want {
			continue
		}
		ok, seen := allowed[b.Project]
		if !seen {
			ok = s.canManage(ctx, b.Project)
			allowed[b.Project] = ok
		}
		if ok {
			resp.Bindings = append(resp.Bindings, bindingProto(b))
		}
	}
	if s.engine.IsOwner(auth.PrincipalFrom(ctx)) && (req.Msg.GetProject() == "" || req.Msg.GetProject() == "*") {
		for _, o := range s.engine.Owners() {
			resp.Bindings = append(resp.Bindings, &evalsiv1alpha1.RoleBinding{Project: "*", Role: OwnerRole, Subject: o, Source: SourceConfig})
		}
	}
	return connect.NewResponse(resp), nil
}

func bindingFrom(msg *evalsiv1alpha1.RoleBinding) (Binding, error) {
	if msg == nil {
		return Binding{}, invalid("binding is required")
	}
	b := Binding{Project: msg.GetProject(), Role: msg.GetRole(), Subject: msg.GetSubject()}
	if b.Project == "" {
		b.Project = DefaultProject
	}
	if err := ValidSubject(b.Subject); err != nil {
		return b, invalid("%v", err)
	}
	return b, nil
}

// CreateBinding implements AuthService.
func (s *Service) CreateBinding(ctx context.Context, req *connect.Request[evalsiv1alpha1.CreateBindingRequest]) (*connect.Response[evalsiv1alpha1.CreateBindingResponse], error) {
	b, err := bindingFrom(req.Msg.GetBinding())
	if err != nil {
		return nil, err
	}
	b.CreatedAt, b.CreatedBy, b.Source = s.now().UTC(), auth.PrincipalFrom(ctx).ID(), SourceAPI
	if err := s.engine.ValidateBinding(b); err != nil {
		return nil, invalid("%v", err)
	}
	if err := s.engine.CheckRoleNames(auth.PrincipalFrom(ctx), b.Project, []string{b.Role}); err != nil {
		return nil, denied(err)
	}
	if err := s.store.CreateBinding(ctx, b); err != nil {
		return nil, storeErr(err, "binding")
	}
	if err := s.engine.Reload(ctx); err != nil {
		return nil, err
	}
	s.record(ctx, "access.manage", b.Project, "binding/"+b.Project+"/"+b.Role, map[string]any{"new": map[string]string{"role": b.Role, "subject": b.Subject}})
	return connect.NewResponse(&evalsiv1alpha1.CreateBindingResponse{Binding: bindingProto(b)}), nil
}

// DeleteBinding implements AuthService.
func (s *Service) DeleteBinding(ctx context.Context, req *connect.Request[evalsiv1alpha1.DeleteBindingRequest]) (*connect.Response[evalsiv1alpha1.DeleteBindingResponse], error) {
	b, err := bindingFrom(req.Msg.GetBinding())
	if err != nil {
		return nil, err
	}
	for _, existing := range s.engine.Bindings() {
		if existing.Project == b.Project && existing.Role == b.Role && existing.Subject == b.Subject && existing.Source == SourceConfig {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this binding is declared in config; remove it there"))
		}
	}
	if err := s.store.DeleteBinding(ctx, b); err != nil {
		return nil, storeErr(err, "such binding")
	}
	if err := s.engine.Reload(ctx); err != nil {
		return nil, err
	}
	s.record(ctx, "access.manage", b.Project, "binding/"+b.Project+"/"+b.Role, map[string]any{"old": map[string]string{"role": b.Role, "subject": b.Subject}})
	return connect.NewResponse(&evalsiv1alpha1.DeleteBindingResponse{}), nil
}

// --- audit ---

// ListAuditEvents implements AuthService. Owners see every event; others
// see the events of projects where they hold audit.read.
func (s *Service) ListAuditEvents(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListAuditEventsRequest]) (*connect.Response[evalsiv1alpha1.ListAuditEventsResponse], error) {
	q := AuditQuery{Action: req.Msg.GetAction(), DeniedOnly: req.Msg.GetDeniedOnly()}
	if s.engine.Enabled() && !s.engine.IsOwner(auth.PrincipalFrom(ctx)) {
		q.Projects = []string{}
		for _, p := range s.engine.Projects() {
			if Can(ctx, "audit.read", p.Name, nil) {
				q.Projects = append(q.Projects, p.Name)
			}
		}
	}
	if want := req.Msg.GetProject(); want != "" {
		if q.Projects != nil && !slices.Contains(q.Projects, want) {
			q.Projects = []string{}
		} else {
			q.Projects = []string{want}
		}
	}
	size := int(req.Msg.GetPageSize())
	if size <= 0 || size > 1000 {
		size = 100
	}
	events, next, err := s.store.ListAudit(ctx, q, size, req.Msg.GetPageToken())
	if err != nil {
		return nil, invalid("%v", err)
	}
	return connect.NewResponse(&evalsiv1alpha1.ListAuditEventsResponse{Events: events, NextPageToken: next}), nil
}
