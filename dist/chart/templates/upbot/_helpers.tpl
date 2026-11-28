{{/*
Labels of the Upbot templates (cleanup job, API key Secret). The generated templates write theirs inline.
*/}}
{{- define "upbot-operator.labels" -}}
app.kubernetes.io/name: {{ include "upbot-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end }}
