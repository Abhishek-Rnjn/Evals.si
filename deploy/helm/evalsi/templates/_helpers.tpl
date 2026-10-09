{{/* Names are fixed (evalsi-*): one install per namespace, and the
evalsi-crds and evalsi-sandboxd charts refer to them. */}}

{{- define "evalsi.labels" -}}
app.kubernetes.io/part-of: evalsi
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/* An image reference, moved to global.imageRegistry when set: a leading
registry host is replaced, and a reference already under the registry is
kept as it is. */}}
{{- define "evalsi.ref" -}}
{{- $ref := .ref -}}
{{- $reg := trimSuffix "/" .root.Values.global.imageRegistry -}}
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

{{- define "evalsi.image" -}}
{{- include "evalsi.ref" (dict "root" . "ref" (printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag))) -}}
{{- end }}

{{- define "evalsi.natsURL" -}}
{{- default (printf "nats://evalsi-nats.%s.svc:4222" .Release.Namespace) .Values.nats.url -}}
{{- end }}

{{- define "evalsi.serverURL" -}}
{{- printf "%s://evalsi.%s.svc:8080" (ternary "https" "http" .Values.server.tls) .Release.Namespace -}}
{{- end }}

{{/* Where workers lease sandboxes: sandbox.address, else this chart's pool. */}}
{{- define "evalsi.sandboxAddress" -}}
{{- if .Values.sandbox.address -}}
{{- .Values.sandbox.address -}}
{{- else if .Values.sandbox.pool.enabled -}}
{{- printf "tls://evalsi-sandbox-pool.%s.svc:7443" .Release.Namespace -}}
{{- end -}}
{{- end }}

{{/* A non-root, read-only container. */}}
{{- define "evalsi.restricted" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities: {drop: [ALL]}
{{- end }}

{{- define "evalsi.podSecurity" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile: {type: RuntimeDefault}
{{- end }}
