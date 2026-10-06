{{/* Names are fixed (evalsi-*): one install per namespace, and the
evalsi-crds and evalsi-sandboxd charts refer to them. */}}

{{- define "evalsi.labels" -}}
app.kubernetes.io/part-of: evalsi
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/* An image reference, moved to global.imageRegistry when set. */}}
{{- define "evalsi.ref" -}}
{{- $ref := .ref -}}
{{- $reg := .root.Values.global.imageRegistry -}}
{{- if $reg -}}
{{- $parts := splitList "/" $ref -}}
{{- $first := first $parts -}}
{{- if and (gt (len $parts) 1) (or (contains "." $first) (contains ":" $first) (eq $first "localhost")) -}}
{{- $ref = join "/" (rest $parts) -}}
{{- end -}}
{{- $ref = trimPrefix "library/" $ref -}}
{{- printf "%s/%s" (trimSuffix "/" $reg) $ref -}}
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
