package sandbox

// The pod rung (level namespaced, or kernel/vm with a gVisor or Kata
// runtime class): one hardened pod per sandbox, created through the
// Kubernetes API by the process serving SandboxService (the sandbox pool).
//
//   - The pod runs the task's image. An init container copies evalsi-guest
//     from the evalsi image into an emptyDir, and the guest agent becomes the
//     container's command, on the pod network behind a per-sandbox bearer
//     token. Commands, files and limits go through it, as in a microVM.
//   - Hardened: no service-account token, no service links, privilege
//     escalation off, all capabilities dropped except the few package
//     installs need, the RuntimeDefault seccomp profile, resource limits,
//     an optional runtime class.
//   - Network: egress, when the policy allows any, goes through this
//     process's logging egress proxy, which the guest relays to from a
//     loopback port. The evalsi-sandboxes NetworkPolicy (Helm chart) admits
//     ingress to sandbox pods only from the sandbox pool and egress only to
//     it, so a pod cannot reach anything else even if it ignores the proxy.
//   - No snapshots: environments set up once per trial instead.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	guestv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1/guestv1alpha1connect"
)

// PodConfig configures the pod rung.
type PodConfig struct {
	// Namespace for sandbox pods; default: this pod's namespace.
	Namespace string `json:"namespace,omitempty"`
	// The image that holds evalsi-guest (the evalsi image), and the path of
	// the binary in it (default /usr/local/bin/evalsi-guest).
	GuestImage string `json:"guest_image"`
	GuestPath  string `json:"guest_path,omitempty"`
	// For sandboxes whose spec names no image.
	DefaultImage string `json:"default_image,omitempty"`
	// The address sandbox pods reach this process at, for egress: its pod
	// IP. Default: $POD_IP (the Downward API).
	AdvertiseIP string `json:"advertise_ip,omitempty"`
	// A RuntimeClass such as gvisor or kata, and the isolation level it
	// gives (namespaced by default; kernel for gVisor, vm for Kata).
	RuntimeClassName string `json:"runtime_class_name,omitempty"`
	Level            string `json:"level,omitempty"`
	// Run as this user; default: the image's (often root, without
	// capabilities). Set a non-zero uid where Pod Security "restricted"
	// applies, with capabilities []: the workdir and /tmp then become
	// volumes the user can write, seeded with the image's workdir.
	RunAsUser *int64 `json:"run_as_user,omitempty"`
	// Linux capabilities kept; default CHOWN, DAC_OVERRIDE, FOWNER, FSETID,
	// KILL, SETGID, SETUID (what apt and pip need). [] keeps none.
	Capabilities     *[]string         `json:"capabilities,omitempty"`
	NodeSelector     map[string]string `json:"node_selector,omitempty"`
	Tolerations      []map[string]any  `json:"tolerations,omitempty"`
	ImagePullSecrets []string          `json:"image_pull_secrets,omitempty"`
	// CPU limit (Kubernetes quantity); memory comes from the sandbox spec.
	CPU string `json:"cpu,omitempty"`
	// Labels added to sandbox pods.
	Labels map[string]string `json:"labels,omitempty"`
	// How long to wait for a pod to run (image pulls); default 5m.
	StartTimeout string `json:"start_timeout,omitempty"`
	// Set when the cluster's CNI enforces NetworkPolicy, so the isolation
	// report can say the network is enforced, not only the proxy.
	NetworkPolicyEnforced bool `json:"network_policy_enforced,omitempty"`
	// The API server; default: in-cluster (KUBERNETES_SERVICE_HOST, the
	// service-account token and CA).
	APIServer string `json:"api_server,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	CAFile    string `json:"ca_file,omitempty"`
}

const (
	podAgentPort   = 7070
	podTokenEnv    = "EVALSI_GUEST_TOKEN"
	podGuestDir    = "/evalsi/bin"
	saDir          = "/var/run/secrets/kubernetes.io/serviceaccount"
	podLabelKey    = "evals.si/sandbox"
	podDefaultCaps = "CHOWN,DAC_OVERRIDE,FOWNER,FSETID,KILL,SETGID,SETUID"
)

// ErrNoSnapshots: the rung cannot snapshot a sandbox.
var ErrNoSnapshots = errors.New("this sandbox rung does not support snapshots")

func (c *PodConfig) validate() error {
	if c.GuestImage == "" {
		return errors.New("sandbox.pod.guest_image is required (the evalsi image)")
	}
	if c.Level != "" {
		if _, err := ParseLevel(c.Level); err != nil {
			return fmt.Errorf("sandbox.pod.level: %w", err)
		}
	}
	if c.StartTimeout != "" {
		if _, err := time.ParseDuration(c.StartTimeout); err != nil {
			return fmt.Errorf("sandbox.pod.start_timeout: %w", err)
		}
	}
	return nil
}

// kube is the little of the Kubernetes API the rung needs.
type kube struct {
	base      string
	tokenFile string
	hc        *http.Client
}

func newKube(c *PodConfig) (*kube, error) {
	base, tokenFile, caFile := c.APIServer, c.TokenFile, c.CAFile
	if base == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" {
			return nil, errors.New("not in a Kubernetes cluster (no KUBERNETES_SERVICE_HOST) and no sandbox.pod.api_server")
		}
		base = "https://" + net.JoinHostPort(host, port)
		if tokenFile == "" {
			tokenFile = saDir + "/token"
		}
		if caFile == "" {
			caFile = saDir + "/ca.crt"
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // the API server is never reached through an egress proxy
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("kubernetes CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("kubernetes CA: no certificates")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &kube{base: strings.TrimSuffix(base, "/"), tokenFile: tokenFile, hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
}

// do sends a request; the token is re-read every time, since projected
// service-account tokens rotate.
func (k *kube) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if k.tokenFile != "" {
		tok, err := os.ReadFile(k.tokenFile)
		if err != nil {
			return fmt.Errorf("kubernetes token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := k.hc.Do(req)
	if err != nil {
		return fmt.Errorf("kubernetes API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		var status struct{ Message string }
		if json.Unmarshal(msg, &status) == nil && status.Message != "" {
			return fmt.Errorf("kubernetes API: %s %s: %d %s", method, path, resp.StatusCode, status.Message)
		}
		return fmt.Errorf("kubernetes API: %s %s: %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type podStatus struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Phase             string `json:"phase"`
		PodIP             string `json:"podIP"`
		Reason            string `json:"reason"`
		Message           string `json:"message"`
		ContainerStatuses []struct {
			Ready bool `json:"ready"`
			State struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
				Terminated *struct {
					Reason   string `json:"reason"`
					ExitCode int    `json:"exitCode"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type podDriver struct {
	sb   *Sandbox
	cfg  *PodConfig
	kube *kube
	err  error // why the rung cannot run here
}

func newPodDriver(s *Sandbox) *podDriver {
	d := &podDriver{sb: s, cfg: s.cfg.Pod}
	if d.cfg == nil {
		d.err = errors.New("no sandbox.pod section")
		return d
	}
	d.kube, d.err = newKube(d.cfg)
	if d.cfg.Namespace == "" && d.err == nil {
		ns, err := os.ReadFile(saDir + "/namespace")
		if err != nil {
			d.err = errors.New("sandbox.pod.namespace is required outside a pod")
		} else {
			d.cfg.Namespace = strings.TrimSpace(string(ns))
		}
	}
	return d
}

func (d *podDriver) name() string { return "pod" }

func (d *podDriver) level() Level {
	if d.cfg != nil && d.cfg.Level != "" {
		l, _ := ParseLevel(d.cfg.Level)
		return l
	}
	return LevelNamespaced
}

func (d *podDriver) available() error { return d.err }

func (d *podDriver) supports(*Spec) error { return nil }

func (d *podDriver) advertiseIP() string {
	if d.cfg.AdvertiseIP != "" {
		return d.cfg.AdvertiseIP
	}
	if ip := os.Getenv("POD_IP"); ip != "" {
		return ip
	}
	return "127.0.0.1"
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (d *podDriver) manifest(sp *Spec, image, token, egress string) map[string]any {
	c := d.cfg
	guestPath := c.GuestPath
	if guestPath == "" {
		guestPath = "/usr/local/bin/evalsi-guest"
	}
	caps := strings.Split(podDefaultCaps, ",")
	if c.Capabilities != nil {
		caps = *c.Capabilities
	}
	labels := map[string]string{"app.kubernetes.io/name": "evalsi-sandbox", podLabelKey: "true"}
	for k, v := range c.Labels {
		labels[k] = v
	}
	args := []string{podGuestDir + "/evalsi-guest", "agent", "--listen", "tcp://0.0.0.0:" + strconv.Itoa(podAgentPort), "--token-env", podTokenEnv}
	if egress != "" {
		args = append(args, "--egress", egress)
	}
	memMB := sp.Resources.MemoryMB
	if memMB <= 0 {
		memMB = defaultMemoryMB
	}
	limits := map[string]string{"memory": strconv.Itoa(memMB) + "Mi"}
	if c.CPU != "" {
		limits["cpu"] = c.CPU
	}
	containerSec := map[string]any{
		"allowPrivilegeEscalation": false, "privileged": false,
		"capabilities": map[string]any{"drop": []string{"ALL"}, "add": caps},
	}
	restricted := map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}}
	mounts := []map[string]any{{"name": "evalsi-guest", "mountPath": podGuestDir, "readOnly": true}}
	volumes := []map[string]any{{"name": "evalsi-guest", "emptyDir": map[string]any{"sizeLimit": "128Mi"}}}
	initContainers := []map[string]any{{
		"name": "guest", "image": c.GuestImage,
		"command":         []string{guestPath, "install", podGuestDir},
		"volumeMounts":    []map[string]any{{"name": "evalsi-guest", "mountPath": podGuestDir}},
		"securityContext": restricted,
		"resources":       map[string]any{"limits": map[string]string{"memory": "64Mi", "cpu": "200m"}},
	}}
	nonRoot := c.RunAsUser != nil && *c.RunAsUser != 0
	if sp.ReadOnlyRoot {
		containerSec["readOnlyRootFilesystem"] = true
	}
	if sp.ReadOnlyRoot || nonRoot {
		// The workdir and /tmp are volumes: the writable parts of a read-only
		// root, or ones a non-root user can write where the image's own
		// directories belong to root. The image's workdir is copied in
		// first, owned by the pod's user.
		mounts = append(mounts,
			map[string]any{"name": "workdir", "mountPath": sp.Workdir},
			map[string]any{"name": "tmp", "mountPath": "/tmp"})
		volumes = append(volumes,
			map[string]any{"name": "workdir", "emptyDir": map[string]any{}},
			map[string]any{"name": "tmp", "emptyDir": map[string]any{}})
		initContainers = append(initContainers, map[string]any{
			"name": "workdir", "image": image,
			"command": []string{podGuestDir + "/evalsi-guest", "seed", sp.Workdir, "/evalsi/workdir"},
			"volumeMounts": []map[string]any{
				{"name": "evalsi-guest", "mountPath": podGuestDir, "readOnly": true},
				{"name": "workdir", "mountPath": "/evalsi/workdir"},
			},
			"securityContext": restricted,
			"resources":       map[string]any{"limits": limits},
		})
	}
	podSec := map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}}
	if c.RunAsUser != nil {
		podSec["runAsUser"] = *c.RunAsUser
		podSec["runAsNonRoot"] = *c.RunAsUser != 0
	}
	spec := map[string]any{
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"restartPolicy":                 "Never",
		"terminationGracePeriodSeconds": 0,
		"securityContext":               podSec,
		"initContainers":                initContainers,
		"containers": []map[string]any{{
			"name": "sandbox", "image": image, "command": args,
			"env":             []map[string]string{{"name": podTokenEnv, "value": token}},
			"ports":           []map[string]any{{"containerPort": podAgentPort, "name": "agent"}},
			"securityContext": containerSec,
			"resources":       map[string]any{"limits": limits, "requests": limits},
			"volumeMounts":    mounts,
			"readinessProbe": map[string]any{
				"tcpSocket": map[string]any{"port": podAgentPort}, "periodSeconds": 1,
			},
		}},
		"volumes": volumes,
	}
	if c.RuntimeClassName != "" {
		spec["runtimeClassName"] = c.RuntimeClassName
	}
	if len(c.NodeSelector) > 0 {
		spec["nodeSelector"] = c.NodeSelector
	}
	if len(c.Tolerations) > 0 {
		spec["tolerations"] = c.Tolerations
	}
	if len(c.ImagePullSecrets) > 0 {
		var secrets []map[string]string
		for _, s := range c.ImagePullSecrets {
			secrets = append(secrets, map[string]string{"name": s})
		}
		spec["imagePullSecrets"] = secrets
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"generateName": "evalsi-sandbox-", "namespace": c.Namespace, "labels": labels},
		"spec":     spec,
	}
}

// waitRunning waits for the pod's agent to be reachable, or fails with what
// Kubernetes reports (an image that does not pull, a failed init container).
func (d *podDriver) waitRunning(ctx context.Context, name string) (string, error) {
	timeout := 5 * time.Minute
	if d.cfg.StartTimeout != "" {
		timeout, _ = time.ParseDuration(d.cfg.StartTimeout)
	}
	deadline := time.Now().Add(timeout)
	path := "/api/v1/namespaces/" + d.cfg.Namespace + "/pods/" + name
	for {
		var st podStatus
		if err := d.kube.do(ctx, http.MethodGet, path, nil, &st); err != nil {
			return "", err
		}
		s := st.Status
		if s.Phase == "Failed" || s.Phase == "Succeeded" {
			return "", fmt.Errorf("sandbox pod %s stopped: %s %s", name, s.Reason, s.Message)
		}
		for _, cs := range s.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" || w.Reason == "InvalidImageName" || w.Reason == "CreateContainerConfigError") {
				return "", fmt.Errorf("sandbox pod %s: %s: %s", name, w.Reason, w.Message)
			}
			if t := cs.State.Terminated; t != nil {
				return "", fmt.Errorf("sandbox pod %s: the guest agent exited (%s, %d)", name, t.Reason, t.ExitCode)
			}
		}
		if s.Phase == "Running" && s.PodIP != "" && len(s.ContainerStatuses) > 0 && s.ContainerStatuses[0].Ready {
			return s.PodIP, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("sandbox pod %s did not start within %s (phase %q)", name, timeout, s.Phase)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

type tokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

func (d *podDriver) open(ctx context.Context, sp *Spec, dir, from string) (backend, error) {
	if from != "" {
		return nil, ErrNoSnapshots
	}
	image := sp.Image
	if image == "" {
		image = d.cfg.DefaultImage
	}
	if image == "" {
		return nil, errors.New("the pod rung needs an image (or sandbox.pod.default_image)")
	}
	p := &podSandbox{d: d}
	p.driver, p.sp, p.agentEnv = d.name(), sp, true
	egress := ""
	if sp.Network.Mode != NetworkDeny {
		proxy, err := newEgressProxy("tcp", net.JoinHostPort(d.advertiseIP(), "0"), sp.Network.Allow)
		if err != nil {
			return nil, err
		}
		proxy.anyHost = sp.Network.Mode == NetworkAllow
		p.proxy, egress = proxy, proxy.Addr()
	}
	token := randomToken()
	var created podStatus
	if err := d.kube.do(ctx, http.MethodPost, "/api/v1/namespaces/"+d.cfg.Namespace+"/pods", d.manifest(sp, image, token, egress), &created); err != nil {
		p.closeProxy()
		return nil, err
	}
	p.pod = created.Metadata.Name
	ip, err := d.waitRunning(ctx, p.pod)
	if err != nil {
		_ = p.close()
		return nil, err
	}
	h2c := new(http.Protocols)
	h2c.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: h2c, Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}
	p.guest = guestv1alpha1connect.NewGuestAgentServiceClient(&http.Client{Transport: tokenTransport{token: token, base: tr}},
		"http://"+net.JoinHostPort(ip, strconv.Itoa(podAgentPort)), connect.WithGRPC())
	if err := p.waitAgent(ctx); err != nil {
		_ = p.close()
		return nil, err
	}
	enforcement, netNote := "partial", "network: the egress proxy only; NetworkPolicy enforcement not declared (sandbox.pod.network_policy_enforced)"
	if d.cfg.NetworkPolicyEnforced {
		enforcement, netNote = "full", "network: NetworkPolicy admits only the sandbox pool"
	}
	p.iso = Isolation{Driver: d.name(), Level: d.level().String(), Enforcement: enforcement, Notes: []string{
		"pod " + d.cfg.Namespace + "/" + p.pod + ", image " + image,
		"no service-account token; privilege escalation off; seccomp RuntimeDefault",
		netNote,
	}}
	if d.cfg.RuntimeClassName != "" {
		p.iso.Notes = append(p.iso.Notes, "runtime class: "+d.cfg.RuntimeClassName)
	}
	if p.proxy != nil {
		resp, err := p.guest.StartEgress(ctx, connect.NewRequest(&guestv1alpha1.StartEgressRequest{Listen: "127.0.0.1:0"}))
		if err != nil {
			_ = p.close()
			return nil, fmt.Errorf("guest egress forwarder: %w", err)
		}
		p.proxyAddr = resp.Msg.GetListen()
	}
	if err := p.mkdir(ctx, sp.Workdir); err != nil {
		_ = p.close()
		return nil, err
	}
	if len(sp.Files) > 0 {
		files := make([]File, 0, len(sp.Files))
		for name, content := range sp.Files {
			files = append(files, File{Path: name, Content: content})
		}
		if err := p.writeFiles(files); err != nil {
			_ = p.close()
			return nil, err
		}
	}
	return p, nil
}

// podSandbox is one sandbox pod.
type podSandbox struct {
	guestConn
	d   *podDriver
	pod string
	iso Isolation
}

func (p *podSandbox) waitAgent(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := p.guest.Health(ctx, connect.NewRequest(&guestv1alpha1.HealthRequest{}))
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sandbox pod %s: guest agent: %w", p.pod, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (p *podSandbox) isolation() Isolation { return p.iso }
func (p *podSandbox) imageDigest() string  { return "" }

func (p *podSandbox) snapshot(context.Context, string) error { return ErrNoSnapshots }

func (p *podSandbox) closeProxy() {
	if p.proxy != nil {
		p.proxy.Close()
	}
}

func (p *podSandbox) close() error {
	p.closeProxy()
	if p.pod == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := p.d.kube.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+p.d.cfg.Namespace+"/pods/"+p.pod+"?gracePeriodSeconds=0", nil, nil)
	if err != nil && strings.Contains(err.Error(), ": 404") {
		err = nil
	}
	return err
}
