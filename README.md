# escl-autoscan

Put paper into the scanner's document feeder, walk away, and find a PDF in
Paperless-ngx a minute later. No button to press, no app to open.

escl-autoscan watches an eSCL (AirPrint scan) scanner. When paper lands in the
automatic document feeder (ADF), it scans the stack and writes a validated
multi-page PDF into a folder. If that folder is Paperless-ngx's consume
folder, Paperless picks it up.

It also serves a small web page, behind OpenID Connect login. The page shows
the scanner status and the last scans, and has buttons to scan from the
feeder or the flatbed, a gray/color choice and pause/resume. Prometheus
metrics are on a separate port.

The first target device is the Epson WorkForce WF-2960 (eSCL 2.9). Any eSCL
scanner that reports `AdfState` should work; see [Epson notes](#epson-notes)
for the quirks handled here.

- Single static Go binary, no CGO. Three dependencies: go-oidc, x/oauth2 and
  the Prometheus client.
- Runs as uid 65532 on a read-only root filesystem with no capabilities.
- Apache-2.0.

## How it works

### Autoscan state machine

The service polls `GET /eSCL/ScannerStatus` every `POLL_INTERVAL`:

```
             feeder empty seen
 disarmed ──────────────────────▶ armed
    ▲                               │ AdfState=ScannerAdfLoaded and State=Idle
    │                               ▼
    │  paper removed or      settling (AUTOSCAN_SETTLE)
    ├─ scanner busy ◀──────────────┤
    │  (skipped)                    │ still loaded and idle
    │                               ▼
    └─────────────────────────── scanning ──▶ deliver or reject
```

- **Settle time.** People put paper in, straighten it and add a page. The
  scan starts only after `AUTOSCAN_SETTLE` (10s), and only if the feeder is
  still loaded and the scanner still `Idle`. If someone started a copy on the
  panel (`Processing`) or took the paper out, the attempt is skipped.
- **Re-arming.** After any attempt, successful or not, autoscan re-arms only
  after the feeder has reported empty again. A stack is never scanned twice,
  and after a jam the feeder must be cleared first. Only "empty" reports
  between scans count: the feeder may look empty between two sheets of a
  running job. A stack put in while a scan is still running therefore needs
  to be taken out and put back. The service starts
  disarmed, so a stack that is already in the feeder at startup is not
  scanned automatically. Remove and reinsert it, or press "Scan feeder".
- **Sleeping printer.** While the scanner is unreachable (asleep or off), the
  poll interval backs off up to 30s and `scanner_up` is 0. This is logged
  once, not on every poll. The arm state survives this, so paper put into a
  sleeping printer is scanned once it answers again.
- **One scan at a time.** Automatic and manual scans share one lock. A manual
  scan while another runs gets HTTP 409.
- **Pause.** "Pause" stops automatic scans; manual scans still work. The
  initial value comes from `AUTOSCAN_PAUSED` and is not persisted.

### Scan pipeline

1. `POST /eSCL/ScanJobs` with the scan settings. A busy device (503/409)
   gets up to three retries with backoff; before each retry the status is
   read again.
2. `GET <job>/NextDocument` until it answers 404. A 503 means "not ready";
   it is retried 30 times at 1s, as sane-airscan does.
3. Classify the result, with a fixed precedence:
   - **aborted:** NextDocument answered 410, or after the job the device
     lists *this* job as Aborted/Canceled, or the feeder reports a fault
     (jam, mispick, door open, ...). A PDF from an aborted job is never
     delivered, even if it looks complete.
   - **error / busy:** network errors, timeouts, no document at all.
   - **incomplete_pdf:** the job ended normally, but a PDF failed validation.
   - **ok:** everything else.
4. Validate each PDF: `%PDF-` header, `%%EOF` near the end, a `startxref`
   pointing into the file at an xref table with a trailer or at a
   cross-reference stream, and at least one `/Type /Page`.
5. Deliver atomically: write `scan-….part` in `OUTPUT_DIR`, fsync it and the
   directory, then rename it to `scan-YYYYMMDD-HHMMSS.pdf` (time in `TZ`;
   `-2`, `-3`, … on collisions). Mode 0644.
6. Rejected scans go to `FAILED_DIR` as `scan-…-<result>.pdf`, or are
   discarded if it is unset. They never go to `OUTPUT_DIR`.
7. `DELETE` the job. This is best effort; 404 is normal.

On SIGTERM no new scan starts. A running scan may finish for up to 60s; after
that its job is cancelled and deleted on the device.

## Configuration

Every setting is a flag and an environment variable (`-escl-url` /
`ESCL_URL`). A flag wins over the environment. `escl-autoscan --help` lists
them all.

| Variable | Default | Meaning |
|---|---|---|
| `ESCL_URL` | *(required)* | Scanner base URL, e.g. `https://192.0.2.10`. The port is made explicit (`:443`). |
| `ESCL_TLS_FINGERPRINT` | | Pin the scanner's certificate: SHA-256 of the leaf certificate, hex, colons optional. |
| `ESCL_TLS_INSECURE` | `false` | Skip certificate verification entirely. Use the fingerprint instead if you can. |
| `ESCL_TIMEOUT` | `10s` | Timeout for status, capabilities, job creation and deletion. |
| `ESCL_DOCUMENT_TIMEOUT` | `5m` | Timeout for one NextDocument request. The WF-2960 holds it open while the whole stack is scanned. |
| `POLL_INTERVAL` | `3s` | Status poll interval. Backs off up to `POLL_MAX_BACKOFF` while the scanner is unreachable. |
| `POLL_MAX_BACKOFF` | `30s` | Longest poll interval while the scanner is unreachable (switched off or asleep). Lower it if the scanner is usually off and should be noticed sooner after power-on. |
| `AUTOSCAN_ENABLED` | `true` | Scan automatically. With `false`, only the UI buttons scan. |
| `AUTOSCAN_SETTLE` | `10s` | Delay between detecting paper and scanning. |
| `AUTOSCAN_PAUSED` | `false` | Start paused. |
| `WAKE_WORKAROUND` | `false` | On the Epson wake bug, run a tiny flatbed scan and retry the feeder once. |
| `SCAN_RESOLUTION` | `300` | dpi. |
| `SCAN_COLOR_MODE` | `Grayscale8` | `BlackAndWhite1`, `Grayscale8` or `RGB24`. Default for automatic scans and preselected in the UI. |
| `SCAN_INTENT` | `Document` | `Document`, `TextAndGraphic`, `Photo` or `Preview`. |
| `SCAN_WIDTH` | `2550` | Scan width in 1/300 inch (2550 = 8.5 in, the full feeder width). |
| `SCAN_HEIGHT` | `3508` | Scan height in 1/300 inch (3508 = A4, 3300 = Letter). |
| `SCAN_X_OFFSET` | `0` | Horizontal offset in 1/300 inch. |
| `OUTPUT_DIR` | *(required)* | Where finished PDFs go, e.g. `<consume>/scanner`. |
| `FAILED_DIR` | *(discard)* | Keep rejected and partial scans here. Must be outside `OUTPUT_DIR` and `BACKLOG_DIR`; this is checked at startup. |
| `BACKLOG_DIR` | | Paperless consume root. Enables the backlog metrics. |
| `TZ` | `UTC` | Time zone for file names and the UI, e.g. `Europe/Berlin`. The zone database is built in. |
| `HTTP_ADDR` | `:8080` | UI listener. |
| `METRICS_ADDR` | `:9090` | `/metrics`, `/healthz` and `/readyz` listener. |
| `UI_ENABLED` | `true` | Serve the UI. With `false`, the service still autoscans. |
| `UI_NO_AUTH` | `false` | Serve the UI **without login**. Unsafe; see below. |
| `OIDC_ISSUER` | | OpenID Connect issuer URL. Setting it enables login. |
| `OIDC_CLIENT_ID` | | Client ID. |
| `OIDC_CLIENT_SECRET` / `OIDC_CLIENT_SECRET_FILE` | | Client secret, directly or from a file. Leave both empty for a public client (PKCE only). |
| `OIDC_REDIRECT_URL` | | `https://<ui host>/auth/callback`. Its origin is also the only origin allowed for POSTs. |
| `OIDC_SCOPES` | `openid profile email groups` | Separated by spaces or commas. |
| `OIDC_ALLOWED_GROUPS` | | Comma-separated. Login needs one of these in the `groups` claim. Empty allows any authenticated user, with a warning in the log. |
| `SESSION_KEY` / `SESSION_KEY_FILE` | | At least 32 bytes. Signs session cookies. Required with OIDC. |
| `SESSION_TTL` | `12h` | Session lifetime. |
| `COOKIE_INSECURE` | `false` | Drop the `Secure` cookie flag, for local tests over plain HTTP only. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `LOG_FORMAT` | `json` | `json` or `text`. |

Get the scanner's certificate fingerprint with:

```sh
openssl s_client -connect 192.0.2.10:443 </dev/null 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256
```

## Metrics

All metrics are on `METRICS_ADDR` with the prefix `escl_autoscan_`:

| Metric | Meaning |
|---|---|
| `scanner_up` | 1 if the last status poll succeeded. |
| `adf_state{state}` | 1 for the current ADF state, 0 for the others. |
| `armed`, `paused` | Autoscan state. |
| `scans_total{source,result}` | `source` is `feeder` or `platen`. `result` is one of `ok`, `aborted`, `incomplete_pdf`, `busy`, `error`, `wake_glitch` or `skipped`. |
| `pages_total` | Pages delivered. |
| `last_success_timestamp_seconds` | Time of the last delivered scan. |
| `scan_duration_seconds` | Histogram of job durations. |
| `backlog_files`, `backlog_oldest_age_seconds` | Only with `BACKLOG_DIR`. Counts consumable files (`.pdf .png .jpg .jpeg .tif .tiff .webp .txt .eml`) below the consume root, every 60s. `.part` files and dotfiles are ignored. A growing age means Paperless is not consuming. |

`/healthz` reports that the process is alive. `/readyz` checks that
`OUTPUT_DIR` is reachable. Neither depends on the scanner, because a sleeping
printer is normal.

## Epson notes

These come from an Epson WF-2960 running eSCL 2.9, and from sane-airscan.

- **HTTPS only, self-signed.** The device answers eSCL on 443 only; plain
  HTTP returns 404. Pin the certificate with `ESCL_TLS_FINGERPRINT`.
- **Port in the Host header.** Epson needs `Host: <ip>:443`; without the port
  it returns a broken job `Location` (sane-airscan: `quirk_port_in_host`).
  escl-autoscan always puts the port into the URL, so Go sends it. The job
  URL is built from the path of `Location` only, so requests never leave the
  configured host and port.
- **Right-justified feeder.** The ADF reports `XImagePosition=Right`: paper
  runs along the right edge of an 8.5 in path. The default region is the full
  width (2550) and A4 height (3508), so nothing is cropped. A4 pages get about
  6 mm of blank margin on one side. For a tight A4 crop, `SCAN_WIDTH=2480`
  with `SCAN_X_OFFSET=70` should move the region to the right edge;
  sane-airscan uses this offset for right-justified feeders. This has not been
  tested on the device.
- **One PDF per job.** A three-sheet feeder job blocks the first NextDocument
  for about 45 s, then returns one PDF with all pages; the next call returns
  404. Some other devices return one document per page, and then each becomes
  its own file.
- **Truncated PDF on jam.** After a jam, NextDocument returned 200 with a
  truncated PDF (no xref, no trailer), and the next call returned 410. The
  status then showed `Stopped` / `ScannerAdfJam` and the job as Aborted. Such
  a scan is classified `aborted` and never delivered.
- **Wake bug** ([sane-airscan #398](https://github.com/alexpevzner/sane-airscan/issues/398)).
  After waking from standby, a feeder job can end with 404 and no document
  although paper is still loaded. This is detected, logged and counted as
  `wake_glitch`. With `WAKE_WORKAROUND=true` the service runs a tiny 100 dpi
  flatbed scan, throws it away and retries the feeder once.

## Kubernetes

Run exactly one replica: it is the only writer of `scan-*` names in
`OUTPUT_DIR`. `terminationGracePeriodSeconds` must be longer than the 60s a
running scan gets at shutdown. The example uses generic names; adapt the
storage to whatever serves your Paperless consume share.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: escl-autoscan
stringData:
  oidc-client-secret: "<client secret from your identity provider>"
  session-key: "<at least 32 random bytes, e.g. openssl rand -base64 48>"
---
# The Paperless consume share (e.g. NFS), mounted read-write.
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: paperless-consume
spec:
  accessModes: [ReadWriteMany]
  storageClassName: nfs-client
  resources:
    requests:
      storage: 10Gi
---
# Rejected scans, outside the consume share.
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: escl-autoscan-failed
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: escl-autoscan
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: escl-autoscan
  template:
    metadata:
      labels:
        app: escl-autoscan
    spec:
      terminationGracePeriodSeconds: 90
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: escl-autoscan
          image: ghcr.io/snakebiteef2000/escl-autoscan:v0.1.0
          env:
            - { name: ESCL_URL, value: "https://192.0.2.10" }
            - { name: ESCL_TLS_FINGERPRINT, value: "<sha-256 fingerprint>" }
            - { name: OUTPUT_DIR, value: /consume/scanner }
            - { name: BACKLOG_DIR, value: /consume }
            - { name: FAILED_DIR, value: /failed }
            - { name: TZ, value: Europe/Berlin }
            - { name: OIDC_ISSUER, value: "https://id.example.com" }
            - { name: OIDC_CLIENT_ID, value: escl-autoscan }
            - { name: OIDC_REDIRECT_URL, value: "https://scanner.example.com/auth/callback" }
            - { name: OIDC_ALLOWED_GROUPS, value: scanner }
            - name: OIDC_CLIENT_SECRET
              valueFrom: { secretKeyRef: { name: escl-autoscan, key: oidc-client-secret } }
            - name: SESSION_KEY
              valueFrom: { secretKeyRef: { name: escl-autoscan, key: session-key } }
          ports:
            - { name: http, containerPort: 8080 }
            - { name: metrics, containerPort: 9090 }
          readinessProbe:
            httpGet: { path: /readyz, port: metrics }
          livenessProbe:
            httpGet: { path: /healthz, port: metrics }
          resources:
            requests: { cpu: 10m, memory: 32Mi }
            limits: { memory: 512Mi }  # documents are held in memory until validated
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
          volumeMounts:
            - { name: consume, mountPath: /consume }
            - { name: failed, mountPath: /failed }
      volumes:
        - name: consume
          persistentVolumeClaim:
            claimName: paperless-consume
        - name: failed
          persistentVolumeClaim:
            claimName: escl-autoscan-failed
---
apiVersion: v1
kind: Service
metadata:
  name: escl-autoscan
  labels:
    app: escl-autoscan
spec:
  selector:
    app: escl-autoscan
  ports:
    - { name: http, port: 80, targetPort: http }
    - { name: metrics, port: 9090, targetPort: metrics }
```

Publish the `http` port through your Ingress or Gateway as
`https://scanner.example.com`. Keep `metrics` cluster-internal. The pod needs
egress to the scanner (TCP 443) and to the identity provider. NFS usually
ignores `fsGroup`, so make sure uid 65532 can write to `<consume>/scanner`.

In the identity provider (Pocket ID, Keycloak, Authentik, ...), create a
client with the redirect URL above and a `groups` claim in the ID token.

## Paperless-ngx integration

Point `OUTPUT_DIR` at a subfolder of the consume directory and let Paperless
consume recursively:

```sh
PAPERLESS_CONSUMER_RECURSIVE=true
PAPERLESS_CONSUMER_SUBDIRS_AS_TAGS=true   # documents from <consume>/scanner get the tag "scanner"
PAPERLESS_CONSUMER_POLLING_INTERVAL=10    # inotify does not work on NFS; poll every 10s
```

- Paperless only consumes known extensions, so it ignores the `.part` file
  while a scan is being written. The rename makes the PDF appear complete in
  one step.
- Paperless deletes consumed files, so it needs write access to
  `<consume>/scanner`.
- Set `BACKLOG_DIR` to the consume root and alert on
  `escl_autoscan_backlog_oldest_age_seconds`; it catches a stuck consumer.

## Security notes

- **Login.** The OpenID Connect authorization code flow uses PKCE (S256),
  `state` and a `nonce`, all checked on the callback. The ID token is
  verified with the provider's keys (go-oidc), and the `groups` claim (a list
  or a single string) must contain one of `OIDC_ALLOWED_GROUPS`.
- **Sessions** are stateless cookies signed with HMAC-SHA256: subject, name,
  email, groups and expiry. They are `HttpOnly`, `Secure`, `SameSite=Lax`
  and use the `__Host-` prefix. There is no server-side logout of other
  sessions; rotate `SESSION_KEY` to invalidate every session. Login state,
  sessions and CSRF tokens are signed with separate domain tags, so one can
  never pass as another.
- **CSRF.** Every POST needs a per-session token (an HMAC of the session) and
  an `Origin` (or `Referer`) that matches the origin of `OIDC_REDIRECT_URL`.
  Requests with neither header are refused.
- **`UI_NO_AUTH=true` is unsafe.** Anyone who can reach the UI can start
  scans, and see file names and the scanner status. Without OIDC the UI does
  not start unless this is set explicitly, and the service then logs a
  warning. CSRF and origin checks stay on.
- **Metrics** are on their own listener and not reachable through the UI
  port. They contain no document content.
- **Scanner TLS.** Pin the certificate (`ESCL_TLS_FINGERPRINT`) rather than
  using `ESCL_TLS_INSECURE`. Note that the scanner's eSCL interface itself
  has no authentication: anyone on its network can scan from it.
- **Secrets** (client secret, session key) are never logged. Prefer the
  `_FILE` variants or Kubernetes Secrets over command-line flags.
- The container runs as uid 65532 on a read-only root filesystem with no
  capabilities. The UI sends a strict Content-Security-Policy and uses no
  JavaScript.

## Development

```sh
go test -race ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
docker build -t escl-autoscan .
```

The tests run against a fake eSCL server (`internal/escltest`) that mimics
the WF-2960: the Host header with port, absolute job `Location`, 503 retries,
a jam with a truncated PDF and 410, and the wake glitch. Fixtures in
`internal/escltest/testdata` are sanitized captures from the device.

## License

Apache License 2.0, see [LICENSE](LICENSE).
