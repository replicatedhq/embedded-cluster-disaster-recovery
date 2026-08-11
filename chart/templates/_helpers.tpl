{{- define "embedded-cluster-dr.name" -}}
embedded-cluster-dr
{{- end -}}

{{- define "embedded-cluster-dr.serviceAccountName" -}}
{{- default (include "embedded-cluster-dr.name" .) .Values.serviceAccount.name -}}
{{- end -}}

{{- define "embedded-cluster-dr.labels" -}}
app.kubernetes.io/name: {{ include "embedded-cluster-dr.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}
