{{- define "yarilo.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "yarilo.fullname" -}}
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

{{- define "yarilo.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "yarilo.labels" -}}
helm.sh/chart: {{ include "yarilo.chart" . }}
app.kubernetes.io/name: {{ include "yarilo.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: yarilo
{{- end }}

{{/* Call with: (dict "root" . "component" "director") */}}
{{- define "yarilo.componentSelectorLabels" -}}
app.kubernetes.io/name: {{ include "yarilo.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "yarilo.componentLabels" -}}
helm.sh/chart: {{ include "yarilo.chart" .root }}
{{ include "yarilo.componentSelectorLabels" . }}
app.kubernetes.io/version: {{ .root.Values.image.tag | default .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: yarilo
{{- end }}

{{- define "yarilo.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{- define "yarilo.configChecksum" -}}
checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
{{- end }}

{{/*
External TLS volume (client-facing cert, e.g. IMAPS/POP3S on director).
Mounted at /etc/yarilo/tls. Call with secretName string.
*/}}
{{- define "yarilo.externalTLSVolume" -}}
{{- if . }}
- name: tls
  secret:
    secretName: {{ . }}
    optional: false
{{- end }}
{{- end }}

{{- define "yarilo.externalTLSMount" -}}
{{- if . }}
- name: tls
  mountPath: /etc/yarilo/tls
  readOnly: true
{{- end }}
{{- end }}

{{/*
Internal mTLS volume and mount, mounted at /etc/yarilo/internal-tls.
Args: dict "root" $ "itls" <component internalTLS> "role" <role> ["vol" <volume name>].
An empty secretName is the chart-made <release>-<role>-internal-tls (#2132).
*/}}
{{- define "yarilo.internalTLSSecret" -}}
{{- .itls.secretName | default (printf "%s-%s-internal-tls" (include "yarilo.fullname" .root) .role) -}}
{{- end }}
{{- define "yarilo.internalTLSVolume" -}}
{{- if .root.Values.internalTLS.enabled }}
- name: {{ .vol | default "internal-tls" }}
  secret:
    secretName: {{ include "yarilo.internalTLSSecret" . }}
    optional: false
{{- end }}
{{- end }}

{{/*
Whether internal mTLS is on: the one switch, internalTLS.enabled (#2138). Servers
and yarctl's clients key on this, so they cannot disagree.
*/}}
{{- define "yarilo.internalTLSEnabled" -}}
{{- if .Values.internalTLS.enabled -}}
true
{{- end -}}
{{- end }}

{{/*
Refuses components.<name>.internalTLS.enabled: TLS is one switch, and a
per-component flag once turned TLS on in the config without a certificate.
*/}}
{{- define "yarilo.internalTLSNoComponentFlags" -}}
{{- range $name, $comp := .Values.components }}
{{- if and (kindIs "map" $comp) (kindIs "map" $comp.internalTLS) (hasKey $comp.internalTLS "enabled") }}
{{- fail (printf "components.%s.internalTLS.enabled is no longer read: internal TLS is the one switch internalTLS.enabled (#2138)" $name) }}
{{- end }}
{{- end }}
{{- end }}

{{- define "yarilo.internalTLSMount" -}}
{{- if .root.Values.internalTLS.enabled }}
- name: {{ .vol | default "internal-tls" }}
  mountPath: /etc/yarilo/internal-tls
  readOnly: true
{{- end }}
{{- end }}

{{/*
DNS names of a role certificate: the role SAN peers check, the pinned internal
name clients verify, and the director's ring names. Renders a YAML list.
*/}}
{{- define "yarilo.internalTLSNames" -}}
{{- $full := include "yarilo.fullname" .root }}
{{- $names := list (printf "%s.role.yarilo.internal" .role) .serverName }}
{{- if eq .role "director" }}
{{- $names = concat $names (list (printf "%s-director-ring" $full) (printf "%s-director" $full)) }}
{{- end }}
{{- toYaml $names }}
{{- end }}

{{/*
The certificate yarctl presents: admin in backend-api, director-admin in the
director pod (its own API only). Args: dict "root" $ "role" <role>.
*/}}
{{- define "yarilo.adminTLSVolume" -}}
{{- if has .role (include "yarilo.internalTLSRoles" .root | fromYamlArray) }}
- name: admin-tls
  secret:
    secretName: {{ printf "%s-%s-internal-tls" (include "yarilo.fullname" .root) .role }}
{{- end }}
{{- end }}
{{- define "yarilo.adminTLSMount" -}}
{{- if has .role (include "yarilo.internalTLSRoles" .root | fromYamlArray) }}
- name: admin-tls
  mountPath: /etc/yarilo/admin-tls
  readOnly: true
{{- end }}
{{- end }}

{{/*
yarctl in the director pod: its own admin API, as director-admin.
*/}}
{{- define "yarilo.directorConsoleEnv" -}}
{{- $tls := eq (include "yarilo.internalTLSEnabled" .) "true" }}
{{- $dir := ternary "/etc/yarilo/admin-tls" "/etc/yarilo/internal-tls" (has "director-admin" (include "yarilo.internalTLSRoles" . | fromYamlArray)) }}
- name: YARILO_ADMIN_TYPE
  value: director
- name: YARILO_ADMIN_URL
  value: {{ printf "%s://localhost:%v" (ternary "https" "http" $tls) .Values.components.director.api.port }}
- name: YARILO_ADMIN_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ printf "%s-director-api-token" (include "yarilo.fullname" .) }}
      key: token
{{- if $tls }}
- name: YARILO_ADMIN_TLS_CERT
  value: {{ $dir }}/tls.crt
- name: YARILO_ADMIN_TLS_KEY
  value: {{ $dir }}/tls.key
- name: YARILO_ADMIN_TLS_CA
  value: {{ $dir }}/ca.crt
- name: YARILO_ADMIN_TLS_SERVER_NAME
  value: {{ .Values.internalTLS.serverName | default (printf "%s-internal" (include "yarilo.fullname" .)) | quote }}
{{- end }}
{{- end }}

{{/*
Roles whose certificate the chart makes: an enabled component with internal TLS
and no secretName of its own; admin rides along for yarctl and the smoketest.
Renders a YAML list.
*/}}
{{- define "yarilo.internalTLSRoles" -}}
{{- $c := .Values.components }}
{{- $on := .Values.internalTLS.enabled }}
{{- $roles := list }}
{{- $own := dict "auth" $c.auth "warden" $c.warden "locks" $c.locks "dict" $c.dict "director" $c.director
      "backend-api" $c.backendAPI "imap" $c.imap "pop3" $c.pop3 "lmtp" $c.lmtp "managesieve" $c.manageSieve
      "submission" $c.submission "jmap" $c.jmap "imap-login" $c.imapLogin "pop3-login" $c.pop3Login
      "submission-login" $c.submissionLogin "managesieve-login" $c.manageSieveLogin "lmtp-login" $c.lmtpLogin
      "jmap-login" $c.jmapLogin "sasl-login" $c.saslLogin "quota-status" $c.quotaStatus }}
{{- range $role, $comp := $own }}
{{- $comp = $comp | default dict }}
{{- $itls := $comp.internalTLS | default dict }}
{{- if and $comp.enabled $on (not $itls.secretName) (not (and (eq $role "director") ($itls.certificate | default dict).enabled)) }}
{{- $roles = append $roles $role }}
{{- end }}
{{- end }}
{{- $b := $c.backend | default dict }}
{{- $bitls := $b.internalTLS | default dict }}
{{- if and $b.coLocated $on (not $bitls.secretName) }}
{{- $roles = concat $roles (list "imap" "pop3" "lmtp" "managesieve" "submission" "jmap" "fts" "backend-api" "backend-reg") }}
{{- end }}
{{- if $roles }}
{{- $roles = append $roles "admin" }}
{{- $d := $c.director | default dict }}
{{- if and $d.enabled $on }}
{{- $roles = append $roles "director-admin" }}
{{- end }}
{{- end }}
{{- toYaml ($roles | uniq | sortAlpha) }}
{{- end }}

{{/*
Init container that blocks until a TCP host:port accepts connections.
Call: (dict "name" "wait-foo" "host" "hostname" "port" "6379" "image" "busybox:1.36")
*/}}
{{- define "yarilo.initWaitTCP" -}}
- name: {{ .name }}
  image: {{ .image }}
  command:
    - sh
    - -c
    - until nc -z {{ .host | quote }} {{ .port }}; do echo "waiting for {{ .host }}:{{ .port }}"; sleep 2; done
  securityContext:
    allowPrivilegeEscalation: false
    runAsNonRoot: true
    runAsUser: 65534
    seccompProfile:
      type: RuntimeDefault
    capabilities:
      drop: ["ALL"]
{{- end }}

{{/*
Redis hostname for init-container TCP probe.
Bundled → internal ClusterIP DNS; external → parsed from redis.externalUrl.
*/}}
{{- define "yarilo.redisInitHost" -}}
{{- if .Values.redis.bundled -}}
{{- printf "%s-redis.%s.svc.cluster.local" (include "yarilo.fullname" .) .Release.Namespace -}}
{{- else -}}
{{- $u := urlParse .Values.redis.externalUrl -}}
{{- index (splitList ":" $u.host) 0 -}}
{{- end -}}
{{- end }}

{{/*
Redis port for init-container TCP probe.
*/}}
{{- define "yarilo.redisInitPort" -}}
{{- if .Values.redis.bundled -}}
6379
{{- else -}}
{{- $u := urlParse .Values.redis.externalUrl -}}
{{- index (splitList ":" $u.host) 1 -}}
{{- end -}}
{{- end }}

{{/*
Database hostname for init-container TCP probe.
Set database.initAddr to "host:port" to enable. Returns empty when not set.
*/}}
{{- define "yarilo.dbInitHost" -}}
{{- if (.Values.database | default dict).initAddr -}}
{{- index (splitList ":" .Values.database.initAddr) 0 -}}
{{- end -}}
{{- end }}

{{/*
Database port for init-container TCP probe.
*/}}
{{- define "yarilo.dbInitPort" -}}
{{- if (.Values.database | default dict).initAddr -}}
{{- index (splitList ":" .Values.database.initAddr) 1 -}}
{{- end -}}
{{- end }}

{{/*
YARILO_DB_DSN env block — injects DSN from Secret or literal value.
Include in any component that reads passdb/userdb from SQL.
*/}}
{{- define "yarilo.dbEnv" -}}
{{- if (.Values.database | default dict).secretName -}}
- name: YARILO_DB_DSN
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.secretName }}
      key: {{ .Values.database.secretKey | default "dsn" }}
{{- else if (.Values.database | default dict).dsn -}}
- name: YARILO_DB_DSN
  value: {{ .Values.database.dsn | quote }}
{{- end -}}
{{- end }}
{{- define "yarilo.backendAPITokenSecret" -}}
{{- default (printf "%s-backend-api-token" (include "yarilo.fullname" .)) .Values.components.backendAPI.token_secret -}}
{{- end }}
{{- define "yarilo.adminBackendEnv" -}}
{{- $tokenSecret := .Values.components.backendAPI.token_secret }}
{{- if eq $tokenSecret "" }}
{{- $tokenSecret = printf "%s-backend-api-token" (include "yarilo.fullname" .) }}
{{- end }}
{{- /* Key on the GLOBAL internal-TLS condition, not components.backendAPI.* —
       backend-api serves HTTPS whenever internal_tls.enabled (any component TLS)
       is on, which the co-located install sets without a backendAPI block (#954). */}}
{{- $btls := eq (include "yarilo.internalTLSEnabled" .) "true" }}
{{- $scheme := ternary "https" "http" $btls }}
- name: YARILO_ADMIN_TYPE
  value: backend
- name: YARILO_API_URL
  {{- /* Co-located: backend-api runs in THIS pod on the pod IP (no separate
         Service), so reach it over localhost. Legacy: the -backend-api Service.
         https when backend-api serves internal mTLS (#954). */}}
  {{- if .Values.components.backend.coLocated }}
  value: "{{ $scheme }}://localhost:9105"
  {{- else }}
  value: {{ printf "%s://%s-backend-api:9105" $scheme (include "yarilo.fullname" .) }}
  {{- end }}
- name: YARILO_API_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ $tokenSecret }}
      key: token
{{- if $btls }}
  {{- /* yarctl dials backend-api over mTLS: present the client cert, trust the
         internal CA, and verify against the pinned SAN (the URL host is
         localhost/an IP that never matches the cert). Same secret the server
         mounts at /etc/yarilo/internal-tls (#954). */}}
{{- $adminDir := ternary "/etc/yarilo/admin-tls" "/etc/yarilo/internal-tls" (has "admin" (include "yarilo.internalTLSRoles" . | fromYamlArray)) }}
- name: YARILO_ADMIN_TLS_CERT
  value: {{ $adminDir }}/tls.crt
- name: YARILO_ADMIN_TLS_KEY
  value: {{ $adminDir }}/tls.key
- name: YARILO_ADMIN_TLS_CA
  value: {{ $adminDir }}/ca.crt
- name: YARILO_ADMIN_TLS_SERVER_NAME
  value: {{ .Values.internalTLS.serverName | default (printf "%s-internal" (include "yarilo.fullname" .)) | quote }}
{{- end }}
{{- end }}
{{/*
YARILO_ADMIN_URL / YARILO_ADMIN_TOKEN env block for the director admin API.
Read directly by yarilo-admin's director subcommands regardless of
YARILO_ADMIN_TYPE (#755) — distinct from yarilo.adminBackendEnv's
YARILO_API_URL/YARILO_API_TOKEN pair, which is claimed by the backend plane.
*/}}
{{- define "yarilo.adminDirectorEnv" -}}
{{- $tokenSecret := printf "%s-director-api-token" (include "yarilo.fullname" .) }}
- name: YARILO_ADMIN_URL
  value: {{ printf "%s://%s-director-api:%v" (ternary "https" "http" (eq (include "yarilo.internalTLSEnabled" .) "true")) (include "yarilo.fullname" .) .Values.components.director.api.port }}
- name: YARILO_ADMIN_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ $tokenSecret }}
      key: token
{{- end }}

{{/*
yarilo.backendVolumeMounts — the volume mounts shared by every co-located
backend protocol/fts/backend-api container (#788): config, tmp, the shared
readiness emptyDir, the mail PV, and the optional internal-mTLS secret.
Args: dict "root" $ "itls" <internalTLS config>.
*/}}
{{- define "yarilo.backendVolumeMounts" -}}
{{- $root := .root -}}
{{- $readyDir := $root.Values.backend_register.readiness_dir | default "/run/yarilo-ready" -}}
- name: config
  mountPath: /etc/yarilo
  readOnly: true
{{- if $root.Values.virtual_definitions }}
- name: virtual-definitions
  mountPath: /etc/yarilo/virtual
  readOnly: true
{{- end }}
- name: tmp
  mountPath: /tmp
- name: ready
  mountPath: {{ $readyDir }}
{{- if $root.Values.storage.persistence.enabled }}
- name: mail
  mountPath: {{ $root.Values.storage.maildir_root | default "/var/mail/vhosts" }}
{{- end }}
{{- include "yarilo.internalTLSMount" (dict "root" $root "itls" .itls "role" .role "vol" (printf "internal-tls-%s" .role)) }}
{{- /* extraVolumes are rendered on the StatefulSet, so the matching mounts
       belong on every backend container. Without them a volume an operator
       added is present in the pod and mounted nowhere: a passwd-file passdb
       renders into the shared config, the backend reads it, does not find the
       file, and crashloops -- the volume looking configured while doing
       nothing (#1303). Every Deployment in this chart already does this. */}}
{{- with $root.Values.extraVolumeMounts }}
{{ toYaml . | trimSuffix "\n" }}
{{- end }}
{{- end -}}

{{/*
Graceful-shutdown preStop hook (#857): delay SIGTERM so kube removes the pod
from Service endpoints before the process stops accepting, closing the
racing-new-connection window. Pass the ROOT context.
*/}}
{{- define "yarilo.preStopDrain" -}}
lifecycle:
  preStop:
    exec:
      command: ["/bin/sh", "-c", "sleep {{ .Values.gracefulShutdown.preStopSleepSeconds | default 5 }}"]
{{- end -}}

{{/*
Effective log level for one component (#887 follow-up).

Falls back to the installation-wide .Values.logLevel unless the component's name
appears in .Values.logLevelOverrides. Keyed by the component name the container
already reports in YARILO_COMPONENT, so an operator raises verbosity for a single
service without knowing the internal values.yaml key layout:

  logLevelOverrides:
    yarilo-auth: debug

Call: (dict "name" "yarilo-auth" "root" $)
*/}}
{{- define "yarilo.logLevel" -}}
{{- $ovr := .root.Values.logLevelOverrides | default dict -}}
{{- if hasKey $ovr .name -}}
{{- index $ovr .name -}}
{{- else -}}
{{- .root.Values.logLevel -}}
{{- end -}}
{{- end -}}

{{/*
startupProbe for a login pod, replacing its wait-* init containers (#903).

Safe for login pods specifically: none of them dials auth or warden during startup
— those connections are created lazily on the first login (#885/#891) — so the
process comes up regardless and the probe only withholds traffic until the
dependencies answer.

While it fails the pod stays out of the Service endpoints AND liveness is not run,
so a dependency slow to appear cannot cause a restart. It stops after the first
success: a dependency failing later is a runtime error the login reports to the
client, not a reason to pull the pod.

URLs are POSITIONAL. yarctl registers a global --url (Director API) and strips
global flags from argv before a subcommand sees them, so a --url here would be
swallowed and the probe would never pass (#906).

Call: (dict "probe" .Values.components.<c>.startupProbe "root" $ "auth" true "warden" true)
*/}}
{{- define "yarilo.loginStartupProbe" -}}
{{- $p := .probe -}}
{{- $root := .root -}}
{{- if $p.enabled }}
startupProbe:
  exec:
    command:
      - yarctl
      - wait
      - --timeout={{ $p.timeout }}
      {{- if .auth }}
      - http://{{ include "yarilo.fullname" $root }}-auth-telemetry.{{ $root.Release.Namespace }}.svc:8080/readyz
      {{- end }}
      {{- if and .warden $root.Values.components.warden.enabled }}
      - http://{{ include "yarilo.fullname" $root }}-warden-telemetry.{{ $root.Release.Namespace }}.svc:8080/readyz
      {{- end }}
  periodSeconds: {{ $p.periodSeconds }}
  ## failureThreshold x periodSeconds is the whole startup budget. Keep it
  ## generous: exceeding it restarts a pod that is healthy and merely waiting,
  ## which the init container never did.
  failureThreshold: {{ $p.failureThreshold }}
{{- end }}
{{- end -}}

{{/*
startupProbe that waits on raw dependency endpoints via `yarctl wait`, replacing a
backend/shared pod's wait-* init containers (#903). Unlike yarilo.loginStartupProbe
(which targets the auth/warden telemetry /readyz URLs), this takes an explicit list of
targets so a pod can wait on tcp://<db> / tcp://<redis> that have no HTTP endpoint.

The pod must come up WITHOUT dialing the dependency at startup (lazy client, no
os.Exit), or removing the init container swaps clean waiting for CrashLoopBackOff.

Call: (dict "probe" .Values.components.<c>.startupProbe "targets" (list "tcp://host:port" ...))
*/}}
{{- define "yarilo.depStartupProbe" -}}
{{- $p := .probe -}}
{{- if and $p.enabled .targets }}
startupProbe:
  exec:
    command:
      - yarctl
      - wait
      - --timeout={{ $p.timeout }}
      {{- range .targets }}
      - {{ . }}
      {{- end }}
  periodSeconds: {{ $p.periodSeconds }}
  ## failureThreshold x periodSeconds is the whole startup budget. Keep it generous:
  ## exceeding it restarts a pod that is healthy and merely waiting for a dependency.
  failureThreshold: {{ $p.failureThreshold }}
{{- end }}
{{- end -}}

{{/*
yarilo.dictPort — the port yarilo-dict listens on, taken from the listener
value so the container port, the Service and dict_addr cannot disagree.
*/}}
{{- define "yarilo.dictPort" -}}
{{- $listen := .Values.components.dict.listen | default ":9107" -}}
{{- $parts := splitList ":" $listen -}}
{{- index $parts (sub (len $parts) 1) -}}
{{- end }}

{{/*
NetworkPolicy listeners: the matrix of pkg/mtls (#2132) on the network layer
(#2138). Each row: the mtls listener, the pod serving it, its ports, the roles
it accepts. The guard checks the roles against mtls.Allowed both ways.
*/}}
{{- define "yarilo.netpolListeners" -}}
{{- $c := .Values.components }}
{{- $co := $c.backend.coLocated }}
{{- $logins := list "imap-login" "pop3-login" "submission-login" "managesieve-login" "lmtp-login" "jmap-login" }}
{{- $sessions := list "imap" "pop3" "lmtp" "managesieve" }}
{{- $rows := list }}
{{- if $c.auth.enabled }}
{{- $rows = append $rows (dict "listener" "auth-client" "server" "auth" "ports" (list (include "yarilo.portNum" $c.auth.listen)) "roles" (concat (list "imap-login" "pop3-login" "submission-login" "managesieve-login" "jmap-login" "sasl-login" "submission" "admin") $sessions)) }}
{{- if $c.auth.masterListen }}
{{- $rows = append $rows (dict "listener" "auth-master" "server" "auth" "ports" (list (include "yarilo.portNum" $c.auth.masterListen)) "roles" (concat (list "backend-api" "fts" "jmap" "quota-status" "lmtp-login" "admin") $sessions)) }}
{{- end }}
{{- end }}
{{- if $c.warden.enabled }}
{{- $rows = append $rows (dict "listener" "warden" "server" "warden" "ports" (list (include "yarilo.portNum" (($c.warden.service | default dict).listen | default ":9101"))) "roles" (concat (list "auth" "backend-api" "imap") $logins)) }}
{{- end }}
{{- if $c.locks.enabled }}
{{- $rows = append $rows (dict "listener" "locks" "server" "locks" "ports" (list (include "yarilo.portNum" (($c.locks.service | default dict).listen | default ":9104"))) "roles" (concat (list "backend-api" "fts" "jmap" "admin") $sessions)) }}
{{- end }}
{{- if $c.dict.enabled }}
{{- $rows = append $rows (dict "listener" "dict" "server" "dict" "ports" (list (include "yarilo.portNum" ($c.dict.listen | default ":9107"))) "roles" $sessions) }}
{{- end }}
{{- if $c.director.enabled }}
{{- $rows = append $rows (dict "listener" "director" "server" "director" "ports" (list (toString $c.director.directorPort)) "roles" (concat (list "director" "backend-api" "backend-reg") $logins)) }}
{{- $rows = append $rows (dict "listener" "director-api" "server" "director" "ports" (list (toString $c.director.api.port)) "roles" (list "admin" "director-admin")) }}
{{- end }}
{{- if $co }}
{{- $rows = append $rows (dict "listener" "backend-api" "server" "backend" "ports" (list "9105") "roles" (list "admin" "backend-api")) }}
{{- $rows = append $rows (dict "listener" "fts" "server" "backend" "ports" (list (toString $c.fts.port)) "roles" (concat (list "backend-api" "jmap") $sessions)) }}
{{- $proto := list (list "imap" "imap" "10143" "imap-backend" "imap-login") (list "pop3" "pop3" "10110" "pop3-backend" "pop3-login") (list "lmtp" "lmtp" "10024" "lmtp-backend" "lmtp-login") (list "manageSieve" "managesieve" "14190" "managesieve-backend" "managesieve-login") (list "submission" "submission" "10587" "submission-backend" "submission-login") (list "jmap" "jmap" "10443" "jmap-backend" "jmap-login") }}
{{- range $p := $proto }}
{{- if (index $c (index $p 0) | default dict).enabled }}
{{- $rows = append $rows (dict "listener" (index $p 3) "server" "backend" "ports" (list (index $p 2)) "roles" (list (index $p 4))) }}
{{- end }}
{{- end }}
{{- else }}
{{- if $c.backendAPI.enabled }}
{{- $rows = append $rows (dict "listener" "backend-api" "server" "backend-api" "ports" (list "9105") "roles" (list "admin" "backend-api")) }}
{{- end }}
{{- if $c.fts.enabled }}
{{- $rows = append $rows (dict "listener" "fts" "server" "fts" "ports" (list (toString $c.fts.port)) "roles" (concat (list "backend-api" "jmap") $sessions)) }}
{{- end }}
{{- $proto := list (list "imap" "imap" (list "imaps" "imap") "imap-backend" "imap-login") (list "pop3" "pop3" (list "pop3s" "pop3") "pop3-backend" "pop3-login") (list "lmtp" "lmtp" (list "lmtp") "lmtp-backend" "lmtp-login") (list "manageSieve" "managesieve" (list "managesieve") "managesieve-backend" "managesieve-login") (list "submission" "submission" (list "submission" "submissions") "submission-backend" "submission-login") (list "jmap" "jmap" (list "jmap") "jmap-backend" "jmap-login") }}
{{- range $p := $proto }}
{{- $comp := index $c (index $p 0) | default dict }}
{{- if $comp.enabled }}
{{- $ports := list }}
{{- range $l := index $p 2 }}
{{- with (index ($comp.listeners | default dict) $l) }}{{ if or (not (hasKey . "enabled")) .enabled }}{{ $ports = append $ports (toString .containerPort) }}{{ end }}{{ end }}
{{- end }}
{{- $rows = append $rows (dict "listener" (index $p 3) "server" (index $p 1) "ports" $ports "roles" (list (index $p 4))) }}
{{- end }}
{{- end }}
{{- end }}
{{- toYaml $rows }}
{{- end }}

{{/* The port of a listen address such as ":9100" or "0.0.0.0:9100". */}}
{{- define "yarilo.portNum" -}}
{{- regexFind "[0-9]+$" (toString .) -}}
{{- end }}

{{/*
The pod a role runs in. Args: dict "root" $ "role" <role>; empty for a role
that reaches its server over loopback or does not exist in this layout.
*/}}
{{- define "yarilo.netpolRolePod" -}}
{{- $co := .root.Values.components.backend.coLocated }}
{{- $inBackend := list "imap" "pop3" "lmtp" "managesieve" "submission" "jmap" "fts" "backend-api" "backend-reg" }}
{{- if eq .role "director-admin" -}}
{{- else if eq .role "admin" -}}
{{ ternary "backend" "backend-api" $co }}
{{- else if and $co (has .role $inBackend) -}}
backend
{{- else if eq .role "backend-reg" -}}
{{- else -}}
{{ .role }}
{{- end -}}
{{- end }}

{{/*
The Redis password, from redis.passwordSecret, for every process that opens a
Redis client or a redis dict; the config names it as ${YARILO_REDIS_PASSWORD}.
*/}}
{{- define "yarilo.redisPasswordEnv" -}}
{{- with (.Values.redis.passwordSecret | default dict).name }}
- name: YARILO_REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ . }}
      key: {{ $.Values.redis.passwordSecret.key | default "password" }}
{{- end }}
{{- end }}
