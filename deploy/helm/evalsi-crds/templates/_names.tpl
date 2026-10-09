{{/* The prefix of the cluster-scoped objects this chart creates. A release
named "evalsi-crds" (the usual one) gives "evalsi"; any other release gets
"<release>-evalsi" after dropping a trailing "-crds". Set fullnameOverride to
choose it. */}}
{{- define "crds.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 40 | trimSuffix "-" -}}
{{- else -}}
{{- $n := .Release.Name | trimSuffix "-crds" -}}
{{- if contains "evalsi" $n -}}{{ $n | trunc 40 | trimSuffix "-" }}{{- else -}}{{ printf "%s-evalsi" $n | trunc 40 | trimSuffix "-" }}{{- end -}}
{{- end -}}
{{- end }}
