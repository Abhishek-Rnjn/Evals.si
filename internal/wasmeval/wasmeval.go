// Package wasmeval runs evaluators compiled to WebAssembly inside evalsid.
//
// A Wasm plugin is a WASI (preview 1) command module plus a manifest
// (evalsi-plugin.yaml) that pins the module's sha256 and describes its
// evaluators. The module gets no filesystem, no network, no environment
// and no arguments beyond the call, a fixed clock and a seeded random
// source, a memory cap and a deadline: it can only read its input and
// write its answer, so untrusted community evaluators run without a
// sandbox rung and give the same answer every time.
//
// The ABI ("evalsi wasm ABI 1") is JSON over stdio. evalsid starts the
// module's _start with argv [plugin-name, "evaluate"] and writes
//
//	{"abi": 1, "evaluator": "acme/json-valid", "params": {...}, "records": [Record, ...]}
//
// to stdin; the module writes {"results": [EvaluationResult, ...]} (one per
// record, in order) to stdout and exits 0. Dataset-scope evaluators get
// "reduce" and answer {"scores": [Score, ...]}. Records, results and scores
// use the protobuf JSON mapping of evalsi.v1alpha1. A non-zero exit fails
// the call with the module's stderr.
package wasmeval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"sigs.k8s.io/yaml"

	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// ABI is the version of the stdio protocol.
const ABI = 1

// ManifestFile is a plugin directory's manifest.
const ManifestFile = "evalsi-plugin.yaml"

// Limits bound one call into a module.
type Limits struct {
	MemoryMB int           `json:"memory_mb"`
	Timeout  time.Duration `json:"-"`
	// Bytes a module may write to stdout (its answer) and stderr.
	MaxOutput int `json:"-"`
}

// Defaults and ceilings; a manifest may ask for less, never more.
var (
	DefaultLimits = Limits{MemoryMB: 64, Timeout: 10 * time.Second, MaxOutput: 16 << 20}
	MaxLimits     = Limits{MemoryMB: 512, Timeout: 120 * time.Second, MaxOutput: 64 << 20}
)

// Manifest is evalsi-plugin.yaml.
type Manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// "native", "wrapped" or "community" (the default for Wasm plugins).
	Tier     string `json:"tier,omitempty"`
	Homepage string `json:"homepage,omitempty"`
	License  string `json:"license,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Runtime  struct {
		Wasm *struct {
			// Relative to the manifest.
			Module string `json:"module"`
			SHA256 string `json:"sha256"`
		} `json:"wasm,omitempty"`
	} `json:"runtime"`
	Limits *struct {
		MemoryMB int    `json:"memory_mb,omitempty"`
		Timeout  string `json:"timeout,omitempty"`
	} `json:"limits,omitempty"`
	Evaluators []json.RawMessage `json:"evaluators"`
}

// Plugin is a loaded, compiled module and the evaluators it hosts.
type Plugin struct {
	Manifest   Manifest
	Dir        string
	Evaluators []*evalsiv1alpha1.EvaluatorManifest
	limits     Limits
	runtime    wazero.Runtime
	module     wazero.CompiledModule
}

// Host compiles plugins and runs calls into them. Each plugin has a
// runtime of its own, so its memory cap is its own.
type Host struct {
	cache   wazero.CompilationCache
	mu      sync.RWMutex
	plugins []*Plugin
	byName  map[string]*Plugin
}

// NewHost makes a host. cacheDir (optional) keeps compiled modules across
// restarts.
func NewHost(cacheDir string) (*Host, error) {
	h := &Host{byName: map[string]*Plugin{}}
	if cacheDir != "" {
		c, err := wazero.NewCompilationCacheWithDir(cacheDir)
		if err != nil {
			return nil, fmt.Errorf("wasm compilation cache: %w", err)
		}
		h.cache = c
	}
	return h, nil
}

func (h *Host) newRuntime(ctx context.Context, l Limits) (wazero.Runtime, error) {
	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(uint32(l.MemoryMB) * 16) // 64 KiB pages
	if h.cache != nil {
		cfg = cfg.WithCompilationCache(h.cache)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	return rt, nil
}

// Close releases the plugins' runtimes.
func (h *Host) Close(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var err error
	for _, p := range h.plugins {
		err = errors.Join(err, p.runtime.Close(ctx))
	}
	if h.cache != nil {
		err = errors.Join(err, h.cache.Close(ctx))
	}
	return err
}

// shortScoreTypes and scopes let manifests say "passed" and "record".
var (
	scoreTypes = map[string]string{"number": "SCORE_TYPE_NUMBER", "passed": "SCORE_TYPE_PASSED",
		"label": "SCORE_TYPE_LABEL", "structured": "SCORE_TYPE_STRUCTURED"}
	scopes = map[string]string{"span": "SCOPE_SPAN", "record": "SCOPE_RECORD", "session": "SCOPE_SESSION", "dataset": "SCOPE_DATASET"}
)

// evaluatorManifest reads one evaluators[] entry (snake_case, short enum
// names) into the proto.
func evaluatorManifest(raw json.RawMessage) (*evalsiv1alpha1.EvaluatorManifest, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if s, ok := doc["scope"].(string); ok {
		if full, ok := scopes[strings.ToLower(s)]; ok {
			doc["scope"] = full
		}
	}
	if outs, ok := doc["outputs"].([]any); ok {
		for _, o := range outs {
			if m, ok := o.(map[string]any); ok {
				if t, ok := m["type"].(string); ok {
					if full, ok := scoreTypes[strings.ToLower(t)]; ok {
						m["type"] = full
					}
				}
			}
		}
	}
	b, _ := json.Marshal(doc)
	m := &evalsiv1alpha1.EvaluatorManifest{}
	if err := protojson.Unmarshal(b, m); err != nil {
		return nil, err
	}
	return m, nil
}

// ReadManifest parses a manifest without loading the module.
func ReadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.UnmarshalStrict(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// Load reads, verifies and compiles the plugin whose manifest is at path.
func (h *Host) Load(ctx context.Context, path string) (*Plugin, error) {
	m, err := ReadManifest(path)
	if err != nil {
		return nil, err
	}
	fail := func(format string, args ...any) (*Plugin, error) {
		return nil, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...))
	}
	if m.Name == "" || m.Version == "" {
		return fail("name and version are required")
	}
	if m.Runtime.Wasm == nil || m.Runtime.Wasm.Module == "" {
		return fail("runtime.wasm.module is required")
	}
	if len(m.Runtime.Wasm.SHA256) != 64 {
		return fail("runtime.wasm.sha256 must pin the module (64 hex characters; evalsid wasm pin writes it)")
	}
	if len(m.Evaluators) == 0 {
		return fail("no evaluators")
	}
	p := &Plugin{Manifest: *m, Dir: filepath.Dir(path), limits: DefaultLimits}
	if l := m.Limits; l != nil {
		if l.MemoryMB > 0 {
			p.limits.MemoryMB = min(l.MemoryMB, MaxLimits.MemoryMB)
		}
		if l.Timeout != "" {
			d, err := time.ParseDuration(l.Timeout)
			if err != nil || d <= 0 {
				return fail("limits.timeout: %q is not a positive duration", l.Timeout)
			}
			p.limits.Timeout = min(d, MaxLimits.Timeout)
		}
	}
	tier := m.Tier
	if tier == "" {
		tier = "community"
	}
	for i, raw := range m.Evaluators {
		em, err := evaluatorManifest(raw)
		if err != nil {
			return fail("evaluators[%d]: %v", i, err)
		}
		if em.GetName() == "" || !strings.Contains(em.GetName(), "/") {
			return fail("evaluators[%d]: name must be namespaced, like acme/json-valid", i)
		}
		if em.GetScope() == evalsiv1alpha1.Scope_SCOPE_UNSPECIFIED {
			em.Scope = evalsiv1alpha1.Scope_SCOPE_RECORD
		}
		if len(em.GetOutputs()) == 0 {
			return fail("evaluators[%d]: outputs are required", i)
		}
		if em.GetRequires().GetJudge() || em.GetRequires().GetIsolation() > evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_NONE {
			return fail("evaluators[%d]: a Wasm evaluator cannot use a judge or a sandbox (it has no I/O)", i)
		}
		if em.GetVersion() == "" {
			em.Version = m.Version
		}
		if em.GetParamsSchema() == nil {
			em.ParamsSchema, _ = structpb.NewStruct(map[string]any{"type": "object"})
		}
		em.Pack, em.Tier, em.Runtime = m.Name, tier, "wasm"
		em.Scheduling = &evalsiv1alpha1.Scheduling{Pool: "wasm"}
		p.Evaluators = append(p.Evaluators, em)
	}
	modPath := m.Runtime.Wasm.Module
	if !filepath.IsAbs(modPath) {
		modPath = filepath.Join(p.Dir, modPath)
	}
	code, err := os.ReadFile(modPath)
	if err != nil {
		return fail("%v", err)
	}
	sum := sha256.Sum256(code)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, m.Runtime.Wasm.SHA256) {
		return fail("module sha256 is %s, the manifest pins %s", got, m.Runtime.Wasm.SHA256)
	}
	if p.runtime, err = h.newRuntime(ctx, p.limits); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = p.runtime.Close(context.Background())
		}
	}()
	if p.module, err = p.runtime.CompileModule(ctx, code); err != nil {
		return fail("compiling %s: %v", m.Runtime.Wasm.Module, err)
	}
	if _, has := p.module.ExportedFunctions()["_start"]; !has {
		return fail("%s is not a WASI command module (no _start export)", m.Runtime.Wasm.Module)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, em := range p.Evaluators {
		if other, dup := h.byName[em.GetName()]; dup {
			return fail("evaluator %s is also defined by plugin %s", em.GetName(), other.Manifest.Name)
		}
	}
	ok = true
	for _, em := range p.Evaluators {
		h.byName[em.GetName()] = p
	}
	h.plugins = append(h.plugins, p)
	return p, nil
}

// LoadDirs loads every plugin under dirs: a directory holding a manifest,
// or a directory of such directories (any depth). Missing dirs are skipped.
func (h *Host) LoadDirs(ctx context.Context, dirs []string) error {
	var paths []string
	for _, dir := range dirs {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && d.Name() == ManifestFile {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		if _, err := h.Load(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// Manifests lists every loaded evaluator.
func (h *Host) Manifests() []*evalsiv1alpha1.EvaluatorManifest {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []*evalsiv1alpha1.EvaluatorManifest
	for _, p := range h.plugins {
		out = append(out, p.Evaluators...)
	}
	return out
}

// Has reports whether an evaluator runs here.
func (h *Host) Has(evaluator string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.byName[evaluator]
	return ok
}

// capped is a writer that fails once it has taken max bytes.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
	// stop ends the call once the limit is passed.
	stop func()
}

var errOutput = errors.New("output limit exceeded")

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		if !c.over && c.stop != nil {
			c.stop()
		}
		c.over = true
		return 0, errOutput
	}
	return c.buf.Write(p)
}

// seeded is the module's random source: the same bytes every call.
type seeded struct{ state uint64 }

func (s *seeded) Read(p []byte) (int, error) {
	for i := range p {
		// xorshift64*
		s.state ^= s.state >> 12
		s.state ^= s.state << 25
		s.state ^= s.state >> 27
		p[i] = byte((s.state * 2685821657736338717) >> 56)
	}
	return len(p), nil
}

// call runs the module once with a request and returns its stdout.
func (h *Host) call(ctx context.Context, p *Plugin, verb string, req any) ([]byte, error) {
	in, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.limits.Timeout)
	defer cancel()
	stdout := &capped{max: p.limits.MaxOutput, stop: cancel}
	stderr := &capped{max: 64 << 10}
	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous: calls run concurrently
		WithArgs(p.Manifest.Name, verb).
		WithStdin(bytes.NewReader(in)).
		WithStdout(stdout).
		WithStderr(stderr).
		WithRandSource(&seeded{state: 0x9E3779B97F4A7C15}).
		WithStartFunctions() // started below, so the exit code can be read
	mod, err := p.runtime.InstantiateModule(ctx, p.module, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: instantiating (memory limit %d MB): %w", p.Manifest.Name, p.limits.MemoryMB, err)
	}
	defer mod.Close(context.Background())
	_, err = mod.ExportedFunction("_start").Call(ctx)
	var exit *sys.ExitError
	switch {
	case stdout.over:
		return nil, fmt.Errorf("%s: wrote more than the %d MB output limit", p.Manifest.Name, p.limits.MaxOutput>>20)
	case err == nil, errors.As(err, &exit) && exit.ExitCode() == 0:
	case ctx.Err() != nil:
		return nil, fmt.Errorf("%s: timed out after %s", p.Manifest.Name, p.limits.Timeout)
	case errors.As(err, &exit):
		return nil, fmt.Errorf("%s: exit code %d: %s", p.Manifest.Name, exit.ExitCode(), p.explain(stderr.buf.String()))
	default:
		return nil, fmt.Errorf("%s: %w: %s", p.Manifest.Name, err, p.explain(stderr.buf.String()))
	}
	return stdout.buf.Bytes(), nil
}

// explain summarizes a failed call's stderr: its start, where runtimes put
// the cause (a Go crash follows it with every goroutine's stack).
func (p *Plugin) explain(stderr string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return "(no stderr)"
	}
	if len(s) > 1000 {
		s = s[:1000] + "..."
	}
	if strings.Contains(stderr, "out of memory") {
		s = fmt.Sprintf("out of memory (limit %d MB): %s", p.limits.MemoryMB, s)
	}
	return s
}

type wasmRequest struct {
	ABI       int               `json:"abi"`
	Evaluator string            `json:"evaluator"`
	Params    json.RawMessage   `json:"params"`
	Records   []json.RawMessage `json:"records"`
}

var marshal = protojson.MarshalOptions{UseProtoNames: false}

func (h *Host) request(evaluator string, params *structpb.Struct, records []*evalsiv1alpha1.Record) (*Plugin, wasmRequest, error) {
	h.mu.RLock()
	p := h.byName[evaluator]
	h.mu.RUnlock()
	if p == nil {
		return nil, wasmRequest{}, fmt.Errorf("no Wasm evaluator %s", evaluator)
	}
	req := wasmRequest{ABI: ABI, Evaluator: evaluator, Params: json.RawMessage("{}")}
	if params != nil {
		b, err := marshal.Marshal(params)
		if err != nil {
			return nil, req, err
		}
		req.Params = b
	}
	for _, r := range records {
		b, err := marshal.Marshal(r)
		if err != nil {
			return nil, req, err
		}
		req.Records = append(req.Records, b)
	}
	return p, req, nil
}

var unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// Evaluate runs a record-scope evaluator over a batch.
func (h *Host) Evaluate(ctx context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	p, wreq, err := h.request(req.GetEvaluator(), req.GetParams(), req.GetRecords())
	if err != nil {
		return nil, err
	}
	start := time.Now()
	out, err := h.call(ctx, p, "evaluate", wreq)
	if err != nil {
		return nil, err
	}
	resp := &pluginv1alpha1.EvaluateResponse{}
	if err := unmarshal.Unmarshal(out, resp); err != nil {
		return nil, fmt.Errorf("%s: invalid answer: %w", p.Manifest.Name, err)
	}
	if len(resp.GetResults()) != len(req.GetRecords()) {
		return nil, fmt.Errorf("%s: %d results for %d records", p.Manifest.Name, len(resp.GetResults()), len(req.GetRecords()))
	}
	per := time.Since(start) / time.Duration(max(len(req.GetRecords()), 1))
	for i, r := range resp.GetResults() {
		r.RecordId = req.GetRecords()[i].GetId()
		r.EvaluatorRef = req.GetEvaluator() + "@" + p.versionOf(req.GetEvaluator())
		if r.GetOutcome() == evalsiv1alpha1.Outcome_OUTCOME_UNSPECIFIED {
			r.Outcome = evalsiv1alpha1.Outcome_OUTCOME_SCORED
		}
		if r.Duration == nil {
			r.Duration = durationpb.New(per)
		}
	}
	resp.BatchId = req.GetBatchId()
	return resp, nil
}

// Reduce runs a dataset-scope evaluator over all records.
func (h *Host) Reduce(ctx context.Context, req *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	p, wreq, err := h.request(req.GetEvaluator(), req.GetParams(), req.GetRecords())
	if err != nil {
		return nil, err
	}
	out, err := h.call(ctx, p, "reduce", wreq)
	if err != nil {
		return nil, err
	}
	resp := &pluginv1alpha1.ReduceResponse{}
	if err := unmarshal.Unmarshal(out, resp); err != nil {
		return nil, fmt.Errorf("%s: invalid answer: %w", p.Manifest.Name, err)
	}
	return resp, nil
}

func (p *Plugin) versionOf(evaluator string) string {
	for _, e := range p.Evaluators {
		if e.GetName() == evaluator {
			return e.GetVersion()
		}
	}
	return p.Manifest.Version
}
