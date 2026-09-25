{{/* Namespace for a tenant: `payments` -> `team-payments`. */}}
{{- define "noisy.ns" -}}
team-{{ .name }}
{{- end -}}
