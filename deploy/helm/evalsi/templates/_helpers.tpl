{{/* Every name this chart creates starts with the release's full name, so
several releases can share a namespace beside other charts. A release named
for evalsi (the usual "evalsi") is used as it is; any other release gets
"-evalsi" after it. The evalsi-crds and evalsi-sandboxd charts take this name
as a value (evalsiFullname, fullname) where they refer to it. */}}
{{- define "evalsi.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 40 | trimSuffix "-" -}}
{{- else if contains "evalsi" .Release.Name -}}
{{- .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-evalsi" .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

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
{{- default (printf "nats://%s-nats.%s.svc:4222" (include "evalsi.fullname" .) .Release.Namespace) .Values.nats.url -}}
{{- end }}

{{- define "evalsi.serverURL" -}}
{{- printf "%s://%s.%s.svc:8080" (ternary "https" "http" .Values.server.tls) (include "evalsi.fullname" .) .Release.Namespace -}}
{{- end }}

{{/* Where workers lease sandboxes: sandbox.address, else this chart's pool. */}}
{{- define "evalsi.sandboxAddress" -}}
{{- if .Values.sandbox.address -}}
{{- .Values.sandbox.address -}}
{{- else if .Values.sandbox.pool.enabled -}}
{{- printf "tls://%s-sandbox-pool.%s.svc:7443" (include "evalsi.fullname" .) .Release.Namespace -}}
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

{{/* The storage settings in effect, as JSON: storage.* from the values, with
the throwaway in-namespace MinIO and ClickHouse (devMinio, devClickhouse)
filled in when they are on. Use: include "evalsi.storage" . | fromJson */}}
{{- define "evalsi.storage" -}}
{{- $fn := include "evalsi.fullname" . -}}
{{- $s3 := deepCopy .Values.storage.s3 -}}
{{- if .Values.devMinio.enabled -}}
{{- $_ := set $s3 "endpoint" (printf "%s-dev-minio.%s.svc:9000" $fn .Release.Namespace) -}}
{{- $_ := set $s3 "bucket" (default "evalsi" $s3.bucket) -}}
{{- $_ := set $s3 "credentialsSecret" (printf "%s-dev-minio" $fn) -}}
{{- $_ := set $s3 "insecure" true -}}
{{- $_ := set $s3 "pathStyle" true -}}
{{- end -}}
{{- $ch := deepCopy .Values.storage.clickhouse -}}
{{- if .Values.devClickhouse.enabled -}}
{{- $_ := set $ch "url" (printf "http://%s-dev-clickhouse.%s.svc:8123" $fn .Release.Namespace) -}}
{{- $_ := set $ch "user" "evalsi" -}}
{{- $_ := set $ch "passwordSecret" (dict "name" (printf "%s-dev-clickhouse" $fn) "key" "password") -}}
{{- end -}}
{{- dict "s3" $s3 "clickhouse" $ch | toJson -}}
{{- end }}
