{{/* The name of what this chart creates: the release's name when it already
mentions sandboxd (the usual "evalsi-sandboxd"), else "<release>-sandboxd", so
several releases can share a namespace. */}}
{{- define "sandboxd.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 55 | trimSuffix "-" -}}
{{- else if contains "sandboxd" .Release.Name -}}
{{- .Release.Name | trunc 55 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-sandboxd" .Release.Name | trunc 55 | trimSuffix "-" -}}
{{- end -}}
{{- end }}
