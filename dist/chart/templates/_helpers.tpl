{{/*
Expand the name of the chart.
*/}}
{{- define "fs-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "fs-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Namespace for generated references.
Always uses the Helm release namespace.
*/}}
{{- define "fs-operator.namespaceName" -}}
{{- .Release.Namespace }}
{{- end }}

{{/*
Resource name with proper truncation for Kubernetes 63-character limit.
Takes a dict with:
  - .suffix: Resource name suffix (e.g., "metrics", "webhook")
  - .context: Template context (root context with .Values, .Release, etc.)
Dynamically calculates safe truncation to ensure total name length <= 63 chars.
*/}}
{{- define "fs-operator.resourceName" -}}
{{- $fullname := include "fs-operator.fullname" .context }}
{{- $suffix := .suffix }}
{{- $maxLen := sub 62 (len $suffix) | int }}
{{- if gt (len $fullname) $maxLen }}
{{- printf "%s-%s" (trunc $maxLen $fullname | trimSuffix "-") $suffix | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" $fullname $suffix | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Rewrite the registry host of an image repository to global.imageRegistry, for
air-gapped or mirrored deployments. Mirrors the operator's ApplyRegistry
(api/v1alpha1): replaces the leading host segment (when it looks like a
registry host — it contains a "." or ":") with the global registry; otherwise
prepends it. An empty registry leaves the repository untouched.
Takes a dict with:
  - .repository: the image repository (e.g. ghcr.io/go-faster/fs-operator)
  - .registry: the global registry override (may be empty)
*/}}
{{- define "fs-operator.applyRegistry" -}}
{{- $repo := .repository -}}
{{- $registry := .registry | default "" | trimSuffix "/" -}}
{{- if not $registry -}}
{{- $repo -}}
{{- else -}}
{{- $host := splitList "/" $repo | first -}}
{{- if and (gt (len (splitList "/" $repo)) 1) (or (contains "." $host) (contains ":" $host)) -}}
{{- printf "%s/%s" $registry (trimPrefix (printf "%s/" $host) $repo) -}}
{{- else -}}
{{- printf "%s/%s" $registry $repo -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Fully qualified manager image reference: the repository with the global
registry applied, then a digest if one is pinned, else the tag (defaulting to
the chart appVersion).

A digest wins over the tag — a mirrored tag can be repointed under a running
operator, a digest cannot. Mirrors FSCluster's spec.image handling (see
fscluster.Image); keep the two in step.
*/}}
{{- define "fs-operator.managerImage" -}}
{{- $repo := include "fs-operator.applyRegistry" (dict "repository" .Values.manager.image.repository "registry" (.Values.global).imageRegistry) -}}
{{- if contains "@" $repo -}}
{{- $repo -}}
{{- else if .Values.manager.image.digest -}}
{{- printf "%s@%s" $repo .Values.manager.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (.Values.manager.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end }}

{{/*
ServiceAccount name to use.
If serviceAccount.enabled is false and serviceAccount.name is set, use that name.
Otherwise, use the standard resourceName helper with "controller-manager" suffix.
*/}}
{{- define "fs-operator.serviceAccountName" -}}
{{- if and (not (.Values.serviceAccount.enabled | default true)) .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- include "fs-operator.resourceName" (dict "suffix" "controller-manager" "context" .) }}
{{- end }}
{{- end }}
