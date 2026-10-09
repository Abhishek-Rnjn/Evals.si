{{/* Every name starts with the release's full name: "evalsi-demo" as it is for a
release named for it, else "<release>-evalsi-demo". The run specs in this
directory name the Services evalsi-demo-model, evalsi-demo-deepagents and so on,
so install the release as "evalsi-demo". */}}
{{- define "demo.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 40 | trimSuffix "-" -}}
{{- else if contains "evalsi-demo" .Release.Name -}}
{{- .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-evalsi-demo" .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{- define "demo.labels" -}}
app.kubernetes.io/part-of: evalsi-demo
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/* An image reference, moved to imageRegistry when set: a leading registry
host is replaced, and a reference already under the registry is kept. */}}
{{- define "demo.image" -}}
{{- $ref := .ref -}}
{{- $reg := trimSuffix "/" .root.Values.imageRegistry -}}
{{- if and $reg (not (hasPrefix (printf "%s/" $reg) $ref)) -}}
{{- $parts := splitList "/" $ref -}}
{{- $first := first $parts -}}
{{- if and (gt (len $parts) 1) (or (contains "." $first) (contains ":" $first) (eq $first "localhost")) -}}
{{- $ref = join "/" (rest $parts) -}}
{{- end -}}
{{- $ref = trimPrefix "library/" $ref -}}
{{- printf "%s/%s" $reg $ref -}}
{{- else -}}
{{- $ref -}}
{{- end -}}
{{- end }}

{{/* Where a pod may run: the component's own values, else the chart's. Call with
(dict "root" . "local" .Values.<component>). */}}
{{- define "demo.scheduling" -}}
{{- $nodeSelector := default .root.Values.nodeSelector .local.nodeSelector -}}
{{- $tolerations := default .root.Values.tolerations .local.tolerations -}}
{{- with $nodeSelector }}
nodeSelector: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $tolerations }}
tolerations: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .root.Values.imagePullSecrets }}
imagePullSecrets: {{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}

{{/* Extra pod labels (chart-wide, then the component's). */}}
{{- define "demo.podLabels" -}}
{{- $labels := merge (dict) (default (dict) .local.podLabels) (default (dict) .root.Values.podLabels) -}}
{{- with $labels }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{/* Extra Service labels, for example istio.io/use-waypoint: none. */}}
{{- define "demo.serviceLabels" -}}
{{- $labels := merge (dict) (default (dict) .local.serviceLabels) (default (dict) .root.Values.serviceLabels) -}}
{{- with $labels }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{- define "demo.restricted" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities: {drop: [ALL]}
{{- end }}

{{/* The model the agents call: the mock this chart runs, or the one in agents.model. */}}
{{- define "demo.modelURL" -}}
{{- if .Values.agents.model.baseURL -}}
{{- .Values.agents.model.baseURL -}}
{{- else -}}
{{- printf "http://%s-model:8000/v1" (include "demo.fullname" .) -}}
{{- end -}}
{{- end }}
