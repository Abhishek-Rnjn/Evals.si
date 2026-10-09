package wasmeval

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// Usage is the `evalsid wasm` help.
const Usage = `usage: evalsid wasm <command>

  check MANIFEST                  verify and compile a plugin, list its evaluators
  pin MANIFEST                    write the module's sha256 into the manifest
  run MANIFEST EVALUATOR [--params JSON]
                                  score records (JSON lines on stdin) and print results
  serve                           JSON-lines service for the Python SDK (stdin/stdout)
`

// DefaultCacheDir keeps compiled modules between runs.
func DefaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "evalsi", "wasm")
}

// Main runs `evalsid wasm`.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, Usage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "evalsid wasm: %v\n", err)
		return 1
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, Usage)
		return 0
	case "check":
		if len(args) != 2 {
			fmt.Fprint(stderr, Usage)
			return 2
		}
		h, err := NewHost(DefaultCacheDir())
		if err != nil {
			return fail(err)
		}
		defer h.Close(ctx)
		p, err := h.Load(ctx, args[1])
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "%s %s (%s): memory %d MB, timeout %s\n", p.Manifest.Name, p.Manifest.Version,
			p.Evaluators[0].GetTier(), p.limits.MemoryMB, p.limits.Timeout)
		for _, e := range p.Evaluators {
			fmt.Fprintf(stdout, "  %s@%s  %s\n", e.GetName(), e.GetVersion(), e.GetDescription())
		}
		return 0
	case "pin":
		if len(args) != 2 {
			fmt.Fprint(stderr, Usage)
			return 2
		}
		sum, err := Pin(args[1])
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "pinned %s\n", sum)
		return 0
	case "run":
		fs := flag.NewFlagSet("wasm run", flag.ContinueOnError)
		fs.SetOutput(stderr)
		params := fs.String("params", "{}", "the evaluator's params as a JSON object")
		// Flags may come before, between or after MANIFEST and EVALUATOR.
		var pos []string
		rest := args[1:]
		for {
			if err := fs.Parse(rest); err != nil {
				fmt.Fprint(stderr, Usage)
				return 2
			}
			if fs.NArg() == 0 {
				break
			}
			pos, rest = append(pos, fs.Arg(0)), fs.Args()[1:]
		}
		if len(pos) != 2 {
			fmt.Fprint(stderr, Usage)
			return 2
		}
		return runRecords(ctx, pos[0], pos[1], *params, stdin, stdout, stderr)
	case "serve":
		if err := Serve(ctx, stdin, stdout); err != nil {
			return fail(err)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "evalsid wasm: unknown command %q\n\n%s", args[0], Usage)
		return 2
	}
}

// shaField is the sha256 value, in block or flow style.
var shaField = regexp.MustCompile(`(\bsha256:[ \t]*)["']?[0-9A-Fa-f]*["']?`)

// Pin computes the module's sha256 and writes it into the manifest.
func Pin(manifest string) (string, error) {
	m, err := ReadManifest(manifest)
	if err != nil {
		return "", err
	}
	if m.Runtime.Wasm == nil || m.Runtime.Wasm.Module == "" {
		return "", errors.New("runtime.wasm.module is not set")
	}
	mod := m.Runtime.Wasm.Module
	if !filepath.IsAbs(mod) {
		mod = filepath.Join(filepath.Dir(manifest), mod)
	}
	code, err := os.ReadFile(mod)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(code)
	sum := hex.EncodeToString(s[:])
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return "", err
	}
	if n := len(shaField.FindAllIndex(raw, -1)); n != 1 {
		return "", fmt.Errorf("the manifest needs exactly one sha256 field to update (found %d)", n)
	}
	out := shaField.ReplaceAll(raw, []byte("${1}"+sum))
	return sum, os.WriteFile(manifest, out, 0o644)
}

func runRecords(ctx context.Context, manifest, evaluator, params string, stdin io.Reader, stdout, stderr io.Writer) int {
	h, err := NewHost(DefaultCacheDir())
	if err != nil {
		fmt.Fprintf(stderr, "evalsid wasm: %v\n", err)
		return 1
	}
	defer h.Close(ctx)
	if _, err := h.Load(ctx, manifest); err != nil {
		fmt.Fprintf(stderr, "evalsid wasm: %v\n", err)
		return 1
	}
	ps := &structpb.Struct{}
	if err := protojson.Unmarshal([]byte(params), ps); err != nil {
		fmt.Fprintf(stderr, "evalsid wasm: --params: %v\n", err)
		return 2
	}
	var records []*evalsiv1alpha1.Record
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		r := &evalsiv1alpha1.Record{}
		if err := protojson.Unmarshal(sc.Bytes(), r); err != nil {
			fmt.Fprintf(stderr, "evalsid wasm: record %d: %v\n", len(records)+1, err)
			return 2
		}
		if r.Id == "" {
			r.Id = fmt.Sprint(len(records))
		}
		records = append(records, r)
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(stderr, "evalsid wasm: %v\n", err)
		return 1
	}
	var dataset bool
	for _, m := range h.Manifests() {
		if m.GetName() == evaluator {
			dataset = m.GetScope() == evalsiv1alpha1.Scope_SCOPE_DATASET
		}
	}
	if dataset {
		resp, err := h.Reduce(ctx, &pluginv1alpha1.ReduceRequest{Evaluator: evaluator, Params: ps, Records: records})
		if err != nil {
			fmt.Fprintf(stderr, "evalsid wasm: %v\n", err)
			return 1
		}
		b, _ := protojson.Marshal(resp)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	resp, err := h.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{Evaluator: evaluator, Params: ps, Records: records})
	if err != nil {
		fmt.Fprintf(stderr, "evalsid wasm: %v\n", err)
		return 1
	}
	for _, r := range resp.GetResults() {
		b, _ := protojson.Marshal(r)
		fmt.Fprintln(stdout, string(b))
	}
	return 0
}

// serveRequest is one line of `evalsid wasm serve`.
type serveRequest struct {
	ID       int64           `json:"id"`
	Manifest string          `json:"manifest"`
	Verb     string          `json:"verb"` // "describe", "evaluate" or "reduce"
	Request  json.RawMessage `json:"request,omitempty"`
}

type serveResponse struct {
	ID       int64           `json:"id"`
	Response json.RawMessage `json:"response,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Serve answers JSON-lines requests until stdin closes. Plugins load on
// first use, by manifest path; requests run concurrently and answers carry
// the request's id. The Python SDK keeps one of these per process.
func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	h, err := NewHost(DefaultCacheDir())
	if err != nil {
		return err
	}
	defer h.Close(context.Background())
	type entry struct {
		p   *Plugin
		err error
	}
	var (
		loadMu sync.Mutex
		loaded = map[string]entry{}
		outMu  sync.Mutex
		wg     sync.WaitGroup
	)
	load := func(path string) (*Plugin, error) {
		loadMu.Lock()
		defer loadMu.Unlock()
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if e, ok := loaded[abs]; ok {
			return e.p, e.err
		}
		p, err := h.Load(ctx, abs)
		loaded[abs] = entry{p, err}
		return p, err
	}
	reply := func(r serveResponse) {
		b, _ := json.Marshal(r)
		outMu.Lock()
		defer outMu.Unlock()
		_, _ = out.Write(append(b, '\n'))
	}
	marshal := protojson.MarshalOptions{}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		var req serveRequest
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			reply(serveResponse{Error: "invalid request: " + err.Error()})
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := serveResponse{ID: req.ID}
			p, err := load(req.Manifest)
			if err != nil {
				res.Error = err.Error()
				reply(res)
				return
			}
			var b []byte
			switch req.Verb {
			case "describe":
				var ms []json.RawMessage
				for _, m := range p.Evaluators {
					raw, _ := marshal.Marshal(m)
					ms = append(ms, raw)
				}
				b, err = json.Marshal(map[string]any{"evaluators": ms})
			case "evaluate":
				ereq := &pluginv1alpha1.EvaluateRequest{}
				if err = protojson.Unmarshal(req.Request, ereq); err == nil {
					var resp *pluginv1alpha1.EvaluateResponse
					if resp, err = h.Evaluate(ctx, ereq); err == nil {
						b, err = marshal.Marshal(resp)
					}
				}
			case "reduce":
				rreq := &pluginv1alpha1.ReduceRequest{}
				if err = protojson.Unmarshal(req.Request, rreq); err == nil {
					var resp *pluginv1alpha1.ReduceResponse
					if resp, err = h.Reduce(ctx, rreq); err == nil {
						b, err = marshal.Marshal(resp)
					}
				}
			default:
				err = fmt.Errorf("unknown verb %q", req.Verb)
			}
			if err != nil {
				res.Error = err.Error()
			} else {
				res.Response = b
			}
			reply(res)
		}()
	}
	wg.Wait()
	return sc.Err()
}
