{{/* An image reference, moved to global.imageRegistry when set: a leading
registry host is replaced, and a reference already under the registry is
kept as it is. Same rules as the evalsi chart. */}}
{{- define "sandboxd.ref" -}}
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
