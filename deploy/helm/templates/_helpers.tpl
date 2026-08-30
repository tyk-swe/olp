{{- define "olp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "olp.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "olp.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "olp.labels" -}}
helm.sh/chart: {{ include "olp.chart" . }}
app.kubernetes.io/name: {{ include "olp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: openllmproxy
{{- end -}}

{{- define "olp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "olp.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "olp.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "olp.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "olp.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "olp.componentFullname" -}}
{{- $maxBaseLength := int (sub 62 (len .component)) -}}
{{- $base := include "olp.fullname" .root | trunc $maxBaseLength | trimSuffix "-" -}}
{{- printf "%s-%s" $base .component -}}
{{- end -}}

{{- define "olp.networkPolicyEgress" -}}
{{- $egress := .Values.networkPolicy.egress -}}
{{- $rules := list -}}
{{- if not $egress.restricted -}}
{{- $rules = append $rules dict -}}
{{- else -}}
{{- if $egress.dns.enabled -}}
{{- $dnsPorts := list (dict "port" 53 "protocol" "UDP") (dict "port" 53 "protocol" "TCP") -}}
{{- $rules = append $rules (dict "ports" $dnsPorts) -}}
{{- end -}}
{{- range $peer := list $egress.postgresql $egress.valkey $egress.providers -}}
{{- if $peer.cidrs -}}
{{- $destinations := list -}}
{{- range $cidr := $peer.cidrs -}}
{{- $destinations = append $destinations (dict "ipBlock" (dict "cidr" $cidr)) -}}
{{- end -}}
{{- $ports := list (dict "port" (int $peer.port) "protocol" "TCP") -}}
{{- $rules = append $rules (dict "to" $destinations "ports" $ports) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- toYaml $rules -}}
{{- end -}}

{{- define "olp.observabilityServiceFullname" -}}
{{- $maxBaseLength := int (sub 48 (len .component)) -}}
{{- $base := include "olp.fullname" .root | trunc $maxBaseLength | trimSuffix "-" -}}
{{- printf "%s-%s-observability" $base .component -}}
{{- end -}}
