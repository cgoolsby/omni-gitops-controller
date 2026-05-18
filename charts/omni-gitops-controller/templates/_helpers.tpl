{{/*
Expand the name of the chart.
*/}}
{{- define "omni-gitops-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "omni-gitops-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "omni-gitops-controller.name" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the credentials secret.
*/}}
{{- define "omni-gitops-controller.credentialsSecret" -}}
{{- if .Values.omni.existingSecret }}
{{- .Values.omni.existingSecret }}
{{- else }}
{{- include "omni-gitops-controller.name" . }}-credentials
{{- end }}
{{- end }}
