package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (d *podDriver) addServices(pod map[string]any, sp *Spec) {
	ordered, _ := orderServices(sp.Services) // validated before manifest generation
	init := pod["initContainers"].([]map[string]any)
	hosts := make([]string, 0, len(ordered))
	for _, s := range ordered {
		hosts = append(hosts, s.Name)
		env := []map[string]string{}
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, map[string]string{"name": k, "value": s.Env[k]})
		}
		limits := map[string]string{"memory": strconv.Itoa(s.MemoryMB) + "Mi", "cpu": strconv.Itoa(s.CPUMillis) + "m"}
		init = append(init, map[string]any{
			"name": "svc-" + s.Name, "image": s.Image, "command": s.Command,
			"env": env, "restartPolicy": "Always",
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "privileged": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
			"resources":       map[string]any{"limits": limits, "requests": limits},
			"startupProbe":    map[string]any{"tcpSocket": map[string]any{"port": s.Port}, "periodSeconds": 1, "failureThreshold": 300},
			"readinessProbe":  map[string]any{"tcpSocket": map[string]any{"port": s.Port}, "periodSeconds": 1, "failureThreshold": 1},
		})
	}
	pod["initContainers"] = init
	pod["hostAliases"] = []map[string]any{{"ip": "127.0.0.1", "hostnames": hosts}}
	// A kubelet-enforced hard deadline still stops services when the pool dies.
	pod["activeDeadlineSeconds"] = int64((sp.ServiceLifetime + time.Second - 1) / time.Second)
}

func serviceStates(st *podStatus, specs []ServiceSpec) []ServiceStatus {
	out := make([]ServiceStatus, 0, len(specs))
	for _, s := range specs {
		state := ServiceStatus{Name: s.Name}
		for _, cs := range st.Status.InitContainerStatuses {
			if cs.Name != "svc-"+s.Name {
				continue
			}
			state.Ready, state.Restarts = cs.Ready, cs.RestartCount
			if w := cs.State.Waiting; w != nil && w.Reason != "PodInitializing" && w.Reason != "ContainerCreating" {
				state.Reason = w.Reason
			}
			if t := cs.State.Terminated; t != nil {
				state.Reason = fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
			}
		}
		if st.Status.Phase == "Failed" || st.Status.Phase == "Succeeded" {
			state.Ready = false
			state.Reason = st.Status.Reason
		}
		out = append(out, state)
	}
	return out
}

func (p *podSandbox) services(ctx context.Context, logs bool) ([]ServiceStatus, error) {
	if len(p.sp.Services) == 0 {
		return nil, nil
	}
	var st podStatus
	if err := p.d.kube.do(ctx, http.MethodGet, "/api/v1/namespaces/"+p.d.cfg.Namespace+"/pods/"+p.pod, nil, &st); err != nil {
		return nil, err
	}
	states := serviceStates(&st, p.sp.Services)
	if logs {
		for i := range states {
			path := "/api/v1/namespaces/" + p.d.cfg.Namespace + "/pods/" + p.pod + "/log?container=svc-" + states[i].Name + "&tailLines=100&limitBytes=16384"
			if err := p.d.kube.do(ctx, http.MethodGet, path, nil, &states[i].Logs); err != nil {
				return nil, fmt.Errorf("service logs %s: %w", states[i].Name, err)
			}
		}
	}
	return states, nil
}

func (p *podSandbox) checkServices(ctx context.Context) error {
	states, err := p.services(ctx, false)
	if err != nil {
		return err
	}
	for _, s := range states {
		if !s.Ready || s.Restarts > 0 || s.Reason != "" {
			return fmt.Errorf("service %s is unhealthy (ready=%t, restarts=%d, reason=%s)", s.Name, s.Ready, s.Restarts, s.Reason)
		}
	}
	return nil
}

// A failed/restarted service is infrastructure failure even if a command
// reports a failed assertion. Never turn an unavailable database into a score.
func (p *podSandbox) exec(ctx context.Context, e *Exec, stdout, stderr io.Writer) *ExecResult {
	failure := func(err error) *ExecResult {
		return &ExecResult{Outcome: OutcomeRunnerFailure, ExitCode: -1, Error: err.Error()}
	}
	if err := p.checkServices(ctx); err != nil {
		return failure(err)
	}
	result := p.guestConn.exec(ctx, e, stdout, stderr)
	if result.Outcome != OutcomeRunnerFailure {
		if err := p.checkServices(ctx); err != nil {
			return failure(err)
		}
	}
	return result
}

const serviceOwnerLabel = "evals.si/service-owner"
const serviceInstanceLabel = "evals.si/service-instance"

// A restarted pool has lost all in-memory sessions. Reap only its own service
// pods before allocating a new one; other replicas' trials are never touched.
func (d *podDriver) reapPreviousServices(ctx context.Context) error {
	d.serviceMu.Lock()
	defer d.serviceMu.Unlock()
	if d.serviceReaped {
		return nil
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string
				Labels map[string]string
			}
		}
	}
	base := "/api/v1/namespaces/" + d.cfg.Namespace + "/pods"
	owner := os.Getenv("POD_UID")
	if err := d.kube.do(ctx, http.MethodGet, base+"?labelSelector="+url.QueryEscape(serviceOwnerLabel+"="+owner), nil, &list); err != nil {
		return fmt.Errorf("reconcile service pods: %w", err)
	}
	for _, pod := range list.Items {
		if pod.Metadata.Labels[serviceOwnerLabel] != owner || pod.Metadata.Labels[serviceInstanceLabel] == d.serviceInstance {
			continue
		}
		if err := d.kube.do(ctx, http.MethodDelete, base+"/"+pod.Metadata.Name+"?gracePeriodSeconds=0", nil, nil); err != nil && !strings.Contains(err.Error(), ": 404") {
			return fmt.Errorf("reap service pod %s: %w", pod.Metadata.Name, err)
		}
	}
	d.serviceReaped = true
	return nil
}
