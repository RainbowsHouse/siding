{{/*
Labels of every object the chart renders.
*/}}
{{- define "siding.labels" -}}
app.kubernetes.io/name: siding
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if ne (toString .Chart.AppVersion) "0.0.0" }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
The plugin configuration of one service: the defaults, with each key the
service sets replacing the default's whole. Not a deep merge: a service that
sets `registry.url` must not inherit `registry.file`, which the plugin would
refuse as two sources.

Takes (dict "root" $ "service" <name> "overrides" <map or nil>).
*/}}
{{- define "siding.config" -}}
{{- $config := deepCopy .root.Values.defaults }}
{{- range $key, $value := (default dict .overrides) }}
{{- $_ := set $config $key $value }}
{{- end }}
{{- if hasKey $config "service" }}
{{- fail (printf "services.%s: `service` is the key under `services`, not a setting" .service) }}
{{- end }}
{{- $_ := set $config "service" .service }}
{{- toYaml $config }}
{{- end }}
