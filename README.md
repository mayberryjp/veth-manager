# veth-manager

**Readable Docker network interfaces, with optional traffic monitoring and control.**

veth-manager gives labeled Docker containers recognizable network interface
names, making troubleshooting and traffic reporting easier. Instead of working
with a name like `veth8a3c1f2`, you can identify an interface as `veth_app`.
It runs as a small background service on your Ubuntu Docker host and keeps
names and monitoring registrations up to date as containers are recreated.

## Features

| Feature | What it helps you do |
| --- | --- |
| Readable interface names | Identify containers by name when inspecting network traffic. Supports Docker bridge networks and container-side macvlan naming. |
| Automatic vnStat integration | Register verified live host interfaces and maintain readable reporting aliases when vnStat 2.x is installed. |
| Optional IPFIX monitoring | Export container flow records to your collector using eBPF, without requiring interface renaming. |
| Optional firewall | Apply per-container traffic policies, preview would-be drops in audit mode, and optionally import Sando allow rules. |
| Optional startup ordering | Start existing containers in a chosen sequence once per Docker service startup. This orders start requests, not application readiness. |
| Targeted DHCP-link cleanup | Remove verified orphaned veth pairs left by supported docker-net-dhcp bridge networks after an observation period. |

**You choose what to enable.** Renaming, orphan cleanup, startup ordering,
traffic export and firewalling are opt-in. Unlabeled containers are never
renamed. The default naming strategy, when enabled, repairs the actual primary
interface name and **briefly interrupts the affected container's networking**.
Choose [alternative naming or auto mode](#non-disruptive-naming-and-monitoring)
if that interruption is unacceptable.

## Requirements

- An **Ubuntu host running local, rootful Docker Engine**.
- `amd64` or `arm64` for the Debian release packages.
- `curl`, `sudo` (unless running as root), and standard Ubuntu package tools,
  including systemd. No Go installation is needed for a release package.
- Optional: vnStat 2.x for traffic history and reporting aliases.
- Optional: Linux 6.6+ with TCX/eBPF support for
  [IPFIX monitoring](#optional-ebpf--ipfix-traffic-monitoring) and firewalling.

This is not for Docker Desktop, rootless Docker, remote Docker hosts,
Kubernetes or Swarm overlay networks. See
[supported networks](#macvlan-and-docker-net-dhcp-networks) for details.

Upgrading from [mayberryjp/veth-rename](https://github.com/mayberryjp/veth-rename)?
Follow the [migration instructions](#migrating-from-veth-rename--veth-renamer)
instead of the fresh-install quick start. The old released package and service
were named `veth-renamer`; do not run both services together.

## Quick start

This example is for a **fresh installation** on an Ubuntu Docker host. It
enables readable primary names only; cleanup, startup ordering, IPFIX and the
firewall remain disabled.

### 1. Install

Run on the Docker host:

```bash
curl -fsSL https://raw.githubusercontent.com/mayberryjp/veth-manager/main/install.sh | bash
```

The installer downloads the latest stable Debian release, checks its SHA-256
checksum and installs it. **It does not start or enable the service.** A
published release is required; see [installation options](#installation-options)
for prerequisites, version pinning and download troubleshooting.

This command executes a downloaded script. Download and inspect it first if
you prefer to review the code before running it.

### 2. Label a container

Add `host-veth-name` to a service in your Docker Compose file. For example:

```yaml
services:
  app:
    image: nginx:alpine
    labels:
      host-veth-name: veth_app
```

Apply the change from the directory containing that Compose file:

```bash
docker compose up -d app
```

Changing labels on an existing container requires recreating it, which can
interrupt its service. Choose a name unique across the host, no more than
15 characters long. See [labels](#labels) for multiple-network examples.

### 3. Enable renaming

Edit the installed configuration:

```bash
sudoedit /etc/veth-manager/manager.yaml
```

Set `renaming.enabled` to `true` and leave cleanup disabled:

```yaml
renaming:
  enabled: true

orphan_cleanup:
  enabled: false
  interval: 5m
  min_age: 10m
```

**Primary-name repair briefly interrupts networking when it changes a name.**
For a no-interruption naming strategy, configure
[alternative mode](#install-as-a-host-service) before starting the service.

### 4. Preview without changing anything

```bash
sudo veth-manager --manager-config=/etc/veth-manager/manager.yaml --once --dry-run
```

Check that the JSON output identifies the expected container and requested
name, with no errors. The preview does not rename interfaces or update vnStat;
it is not a substitute for testing connectivity on a disposable workload.

### 5. Start and check the service

```bash
sudo systemctl enable --now veth-manager.service
sudo systemctl is-active veth-manager.service
sudo journalctl -u veth-manager.service --since "5 minutes ago" --no-pager -o cat
ip link show dev veth_app
```

Expect `active` from systemd and the requested interface name from `ip`.
Check the logs for errors and verify that the container still has the
connectivity your application needs. A running service alone does not prove
that naming succeeded. The service also writes its latest container/interface
mapping to `/run/veth-manager/mappings.json`.

## Guide and reference

- [Installation options](#installation-options)
- [Manager configuration and cleanup](#manager-configuration-opt-in-renaming-and-periodic-cleanup)
- [Docker connection and startup ordering](#docker-connection-and-container-startup-order)
- [IPFIX traffic monitoring](#optional-ebpf--ipfix-traffic-monitoring)
- [Firewall and Sando integration](#optional-ebpf-firewall-and-sando-ignore-list-import)
- [Primary-name repair](#controlled-primary-name-repair) and [non-disruptive naming](#non-disruptive-naming-and-monitoring)
- [Choosing a naming mode](#choosing-a-naming-mode)
- [Supported networks](#macvlan-and-docker-net-dhcp-networks) and [container labels](#labels)
- [Migration](#migrating-from-veth-rename--veth-renamer), [upgrades and removal](#upgrade-and-uninstall)
- [Building from source](#build-and-preview) and [manual service installation](#install-as-a-host-service)
- [Operation](#operation), [flags](#flags) and [mapping output](#mapping-output)
- [Host validation](#validation-on-your-ubuntu-host) and [publishing releases](#publishing-releases)

## Installation options

The [quick-start installer](#1-install) detects the Debian architecture
(`amd64` or `arm64`), resolves the latest stable GitHub release, downloads its
`.deb` and `checksums.txt`, verifies the SHA-256 digest and package identity,
and installs it through `apt-get`. It uses `sudo` only for package installation
and optional service control.

If `curl` is missing, install it first:

```bash
sudo apt-get update
sudo apt-get install curl
```

If package dependencies cannot be found, run `sudo apt-get update` before
retrying the installer.

If a release download fails (for example, with a GitHub gateway timeout), the
installer retries that same release asset through GitHub's API. This fallback
requires `python3` to validate release metadata; install it with
`sudo apt-get install python3` if needed. SHA-256 verification and package
identity checks remain mandatory, and installation stops if either download
route or verification fails.

### Start during installation

Installation does not start, enable, or restart the daemon by default. If
configuration is already ready, you can explicitly enable and restart it:

```bash
curl -fsSL https://raw.githubusercontent.com/mayberryjp/veth-manager/main/install.sh | bash -s -- --start
```

`--start` enables and restarts the service, including on upgrades, so the newly
installed binary is used. It does **not** enable renaming or other opt-in
features. Renaming must first be enabled in
`/etc/veth-manager/manager.yaml`; the default naming mode is primary-repair.
Preserve or configure a systemd override if you prefer alternative names.

### Pin a version

For reproducible installs, pin both the script and package to a release:

```bash
curl -fsSL https://raw.githubusercontent.com/mayberryjp/veth-manager/v0.1.1/install.sh | bash -s -- --version v0.1.1
```

These URLs require the source and a release to be published; the `v0.1.1` example
is not a claim that such a release already exists. Replace it with your chosen
published tag. The `main` branch URL tracks the newest installer, while the
default package version is the newest stable release. Explicit `--version`
also supports prerelease tags. Forks can use `--repo OWNER/REPO` with an
installer downloaded from their own repository.

Checksums detect corrupt or mismatched assets. Because the package and
checksums come from the same GitHub release, they do not protect against a
compromised repository or publisher.

## Manager configuration: opt-in renaming and periodic cleanup

The daemon reads `/etc/veth-manager/manager.yaml` by default; use
`--manager-config=/path/to/manager.yaml` to select a different file.
**Renaming and orphan deletion are both disabled unless explicitly enabled.**
An absent default file keeps both disabled; an explicitly selected missing file,
unreadable file, or invalid configuration fails startup. YAML only: unknown and
duplicate keys, wrong types, nulls, aliases, and multiple documents are rejected.
Restart the service after changing configuration; it is not hot-reloaded.

The Debian package installs this administrator-preserved configuration:

```yaml
renaming:
  enabled: false

orphan_cleanup:
  enabled: false
  interval: 5m
  min_age: 10m
```

Set either `enabled` to `true` independently. `renaming.enabled` gates all
primary and alternative-name mutations, including primary-repair state changes
and stale-name reclamation. `--mode` selects the naming strategy but does not
enable it. Existing interface names are not undone when renaming is disabled.
Verified labeled endpoints can still be registered with vnStat and monitored
by the independent IPFIX/firewall configuration without renaming.

**Upgrade action:** to retain automatic renaming, explicitly set
`renaming.enabled: true` before restarting the new binary. Old `--mode` overrides
alone no longer enable renaming. Cleanup remains disabled unless opted in.

Automatic cleanup is daemon-only, runs at startup to begin observation, then
every `orphan_cleanup.interval`, independently of the rename scan interval.
Both durations must be at least 1s, and `min_age` must be at least `interval`.
A pair must remain an identical eligible candidate across successful scans for
at least `min_age`; the first observation never deletes it. Docker events
trigger fresh ownership, link and address checks: unchanged eligible pairs
keep their observation history, while changed or newly owned pairs lose it.
Unrelated container events do not restart every pair's grace period.
Discovery/deletion failures and daemon restarts reset all observations.
No age state is persisted across restarts.

Cleanup is deliberately restricted to the existing
`ghcr.io/claymore666/docker-net-dhcp:*` driver's bridge-mode networks, including
its default bridge mode. The daemon discovers each host's configured bridges;
it does not assume `br-vlan20`. It requires:

- A reciprocal host-visible **veth** pair named `dh-<12 hex digits>` and
  `<12 hex digits>-dh`, with neither peer in another network namespace.
- A disconnected bridge end on an unambiguously owned DHCP bridge and an
  administratively DOWN, unbridged peer, with no carrier on either end.
- No IPv4 or IPv6 addresses on either end.
- No matching endpoint/container ID prefix in any current Docker network's
  endpoint records or any container's attachments, including stopped and
  unlabeled containers.

Ownership and both link identities are reread for **each** deletion, followed
by a final link/address check immediately before the command. Deleting one end
removes the pair; both disappearances must be verified. Live links, ambiguous
or differently named pairs, physical NICs, VLANs, bridges, and ordinary Docker
bridge networks are never cleanup targets. Every verified deletion is logged
with both names and ifindexes; failures are explicit and retried after a fresh
grace period. Renaming and cleanup execute serially in the daemon.

For read-only validation, stop the mutating service and run:

```bash
sudo veth-manager --manager-config=/etc/veth-manager/manager.yaml --dry-run
```

This runs periodic observations and logs eligible pairs as `would delete`
after the grace period, without changing interfaces or vnStat. `--once` cannot
be combined with enabled orphan cleanup because it cannot establish repeated
observation. Do not run a second mutating manager concurrently.

This removes stale kernel links, not DHCP leases or plugin-internal records.
Docker and rtnetlink have no cross-system transaction: checks narrow but cannot
eliminate races with external lifecycle/network managers. Pause lifecycle
automation during initial validation and verify real container connectivity.
Investigate recurring DHCP teardown/name interactions rather than treating
cleanup as a fix for their cause. A reboot normally clears nonpersistent stale
veths but interrupts containers and does not prevent recurrence.

## Docker connection and container startup order

The daemon reads `/etc/veth-manager/docker.yaml` by default. Use
`--docker-config=/path/to/docker.yaml` to select another file. The package
installs this administrator-preserved file with startup management disabled:

```yaml
endpoint: "unix:///var/run/docker.sock"
control_container_start_order: false
start_order: ""
```

To sequence container startup once per Docker daemon startup:

```yaml
endpoint: "unix:///var/run/docker.sock"
control_container_start_order: true
start_order: "container1 container2 container3 *"
```

`control_container_start_order` defaults to `false` and gates only container
startup sequencing, not Docker access for interface reconciliation or
cleanup. Set it to `false` to disable management while preserving `start_order`;
an omitted flag also disables management even when an order is configured.
An empty order starts nothing regardless of the flag.
An enabled empty sequence is also recorded as complete; adding names later
affects the next Docker invocation, not the current one.

**Upgrade from v0.1.2:** a nonempty `start_order` alone no longer enables
management. Explicitly set `control_container_start_order: true` to retain it.

Entries are exact Docker **container names**, not Compose service names; use
`docker ps -a --format '{{.Names}}'` to find them. Docker Compose restart
policies must be set to `restart: "no"` (not `never`) for managed containers.
Disable other lifecycle automation that could start them independently.
This tool starts existing containers; it does not create containers or run
`docker compose up`.

Startup sequencing requires the local, systemd-managed `docker.service`.
The daemon reads its `ActiveState` and `InvocationID` with `systemctl`.
The invocation ID changes when systemd starts or restarts Docker, but not when
veth-manager restarts or a Docker event stream reconnects. Docker must be active
and its API must successfully return the container inventory before planning
any starts. Events and the periodic `--interval` scan (default 30s) recheck the
service identity; an event-stream disconnect alone is not a restart signal.

For each previously unprocessed invocation, the controller captures the
configured sequence of existing container IDs. Stopped containers are started
serially; running containers are marked processed without restarting them.
Each start request must succeed before proceeding; application health or
readiness is **not** awaited. A failed start is logged and retried on later scans,
but successfully processed containers are not started again during those retries.
A captured container that is removed or renamed is logged and skipped rather
than replaced by a newly created container with the same name.

Progress and the completed invocation ID are atomically saved in
`/var/lib/veth-manager/docker-startup.json`. Once complete, there are **no further
container starts for that Docker invocation**, even if a container crashes,
Compose replaces it, the order changes, or veth-manager restarts. Renaming,
cleanup and monitoring continue normally. Only a new Docker service invocation
allows another startup sequence. `--dry-run` reads existing state and logs
pending starts without writing state or starting containers.

**Change from v0.1.3:** this is startup-only sequencing, not continuous crash
recovery. Image upgrades and intentional stops after the startup sequence are
left to Compose or your updater; slow image downloads need no special pause.
On the first enabled run, if no state file exists, the current active Docker
invocation is treated as unprocessed and runs one initial sequence. Retain the
state file across manager restarts and package upgrades. The packaged service
creates its persistent directory with `StateDirectory=veth-manager`; manual
installs must permit writes there. Missing initial state is normal; unreadable
or invalid existing state and failed writes prevent further startup mutations.

Docker and the filesystem do not share a transaction: a process crash between
a confirmed start and its progress write can leave an unrecorded start.
Already-running containers are skipped on recovery, but absolute exactly-once
start-request delivery across that crash window cannot be guaranteed.

`*` expands to all other existing containers, sorted by name, at that position
in the sequence; explicitly named containers are excluded from the wildcard
even if listed after it. Without `*`, unlisted containers are left alone.
Missing names and inventory errors abort the scan before any starts; a start
failure skips later starts in that scan and is logged and retried on subsequent
scans of the pending startup sequence. Earlier successful starts are not rolled
back. This is ordering of
start requests for stopped containers, not dependency enforcement or a
guarantee that earlier containers remain running.

An absent default configuration disables startup management and preserves the
existing socket default. An explicitly selected missing file or invalid file
fails startup. Configuration is YAML only: unknown/duplicate keys, invalid
types, aliases, multiple documents, duplicate names and multiple wildcards are
rejected. Restart the service to reload changes. `--dry-run` logs the sequence
without starting containers when control is enabled.
`control_container_start_order: true` cannot be used with `--once`, which
remains a read-only container lifecycle command. A saved order with control
disabled does not prevent `--once`.

`endpoint` accepts an absolute Unix socket path, `unix:///path/to/docker.sock`,
or `tcp://127.0.0.1:2375` (also DNS and bracketed IPv6 with an explicit port).
An explicitly supplied `--socket` overrides the configured endpoint. The
connection is shared by startup management, event watching, interface
reconciliation and cleanup: it must point to the Docker Engine **on this
Linux host**, not a remote host whose namespaces/interfaces are inaccessible.
TCP is unencrypted and unauthenticated; do not expose Docker port 2375 to
untrusted networks. TLS endpoints are not supported.

The base systemd unit permits Unix sockets only. For TCP, add a drop-in using
`sudo systemctl edit veth-manager.service`:

```ini
[Service]
RestrictAddressFamilies=
RestrictAddressFamilies=AF_UNIX AF_NETLINK AF_INET AF_INET6
```

Then run `sudo systemctl daemon-reload` and restart the service. Preserve any
existing IPFIX/firewall drop-ins and custom arguments.

## Optional eBPF / IPFIX traffic monitoring

IPFIX remains independent of the manager's renaming and cleanup enablement.
Enable the independent exporter
with `--ipfix-config=/etc/veth-manager/ipfix.yaml`. It monitors only verified
host-side veths belonging to containers managed by the existing rename labels,
including DHCP bridge veths. Unlabeled containers, macvlan, shared bridges,
physical NICs and container-side interfaces are excluded. Failed or ambiguous
endpoint validation is not a license to attach a traffic monitor.

Requires Linux **6.6+** with TCX/eBPF support (Ubuntu 24.04's 6.8 kernel on
amd64 is supported), and `CAP_BPF`, `CAP_PERFMON` and `CAP_NET_ADMIN`
(the daemon already requires additional capabilities for renaming).
Without firewalling, monitoring uses read-only packet observation at both host-veth TCX ingress
and egress. It does not replace existing classifiers, change qdiscs, modify
packets, or lower links. Hooks follow ifindexes across primary renames and
are closed on endpoint removal or shutdown. Packets before initial attachment
are not captured; Docker events and periodic reconciliation discover new veths.

Copy and edit the supplied example, then install the opt-in systemd drop-in:

```bash
sudo install -d -m 0755 /etc/veth-manager /etc/systemd/system/veth-manager.service.d
sudo install -m 0644 /usr/share/doc/veth-manager/ipfix.example.yaml /etc/veth-manager/ipfix.yaml
# Set collector and timers in /etc/veth-manager/ipfix.yaml before starting.
sudo install -m 0644 /usr/share/doc/veth-manager/ipfix.conf /etc/systemd/system/veth-manager.service.d/ipfix.conf
sudo systemctl daemon-reload
sudo systemctl restart veth-manager
```

For source installs, use `deploy/ipfix.example.yaml` and `deploy/ipfix.conf`
instead. Preserve any custom naming flags when editing the drop-in's
`ExecStart`. The base unit remains unchanged and needs no extra capabilities
or network address families when monitoring is disabled.

Example configuration (YAML only, unknown/duplicate keys and invalid types
are rejected; restart to reload):

```yaml
collector: "192.0.2.10:4739"
observation_domain_id: 1
active_timeout: 60s
idle_timeout: 15s
poll_interval: 1s
template_refresh: 30s
template_refresh_packets: 20
max_message_size: 464
max_flows_per_veth: 65536
sampling_rate: 1
capture_filter: ""
```

`collector` is required, accepts DNS/IPv4 `host:port` or `[IPv6]:port`, and uses
UDP. Choose a distinct observation domain per exporter host. Other settings
have the defaults above (domain defaults to zero). Durations must be at least
1ms; polling must not exceed either timeout; template refresh is at most 10m.
`template_refresh_packets` is 0..1000000; the default refreshes templates after
20 successful data messages, and zero disables only packet-based refresh.
`max_message_size` is the maximum IPFIX UDP payload, 156..65507 bytes. The
default 464 leaves room for base IPv6/UDP headers within a 512-byte packet.
For a verified 1280-byte effective WireGuard path MTU, 1200 is a suitable
payload limit. Account for any additional encapsulation or IP headers when
choosing a larger value; an interface MTU alone does not prove the path MTU.
Linux nonfragmenting PMTU discovery clamps the configured payload to the
socket's current path estimate, subtracting 28 bytes for IPv4/UDP or 48 for
IPv6/UDP. A path too small for the 156-byte templates is an explicit error.
`sampling_rate` is 1..1000000; 1 observes every packet, 4 examines every fourth
packet. A shared atomic sequence per veth spans ingress and egress, before
parsing or exclusions (including non-IP frames). Counters describe samples,
not scaled totals; the collector can scale using the exported interval.
Collector DNS is resolved at startup. Configuration failures, verifier
failures, attach failures, metering losses and socket errors are explicit
errors in journald; attachment and export errors are retried without changing
rename results. `--once` and `--dry-run` cannot enable monitoring.

**Migration is required for existing JSON configurations.** Convert settings
to YAML, save as `.yaml` or `.yml`, and update the service drop-in's
`--ipfix-config` path before restarting. JSON content is rejected even if
renamed to `.yaml`. Duration fields are strings, numeric fields are integers,
and defaults apply only to omitted settings, not invalid/null values.

`capture_filter` accepts exactly `""` (all IP), `ip` (IPv4), `ip6` (IPv6),
`tcp`, `udp`, `icmp` (IPv4 ICMP), or `icmp6`. Sampling happens before parsing
and filtering. UDP packets destined to the actual resolved collector address
and port are excluded to avoid export feedback; replies and other destination
ports remain eligible. Reconnection uses that same resolved address, so the
kernel exclusion cannot diverge from the export destination.

### Flow and wire semantics

- IPv4/IPv6 **unidirectional** flows, keyed per veth by IP version,
  protocol, addresses, TCP/UDP/SCTP ports and TOS/IPv6 traffic class.
  ICMP/ICMPv6 use zero source port and destination port `(type << 8) | code`.
  Other IP protocols still use zero ports; only fragments are excluded by the
  no-fragment-flow policy. Zero is a valid ICMP encoding (IPv4 echo reply).
  Capture remains on both ingress and egress. Source/destination addresses and
  ports preserve the observed packet direction: A->B and B->A have separate
  counters and expiration timers. The collector correlates opposite flows.
- Each record carries one packet/octet delta counter pair,
  start/end timestamps in Unix milliseconds, `observationPointId`
  (host ifindex), `interfaceName`, and `flowEndReason`: idle (1), active (2),
  or forced end (4) on removal/shutdown. No reverse IEs or `biflowDirection`
  are exported.
  No payload or packet samples are exported.
- Records include `ipClassOfService` (IE 5), ORed eight-bit `tcpControlBits`
  (IE 6), `samplingInterval` (IE 34), and `samplingAlgorithm` (IE 35 = 1,
  deterministic). TCP flags describe only packets in that exported segment;
  acknowledged flags do not leak into later active/idle segments.
- Active and idle exports remove completed segments from the flow table.
  Continued traffic starts a new segment with exact sampled-packet start/end
  timestamps. Packet timestamps use the monotonic clock translated against
  the startup wall clock. Generic active/idle timers are unchanged; there
  are no protocol-specific timers.
- UDP IPFIX version 10 messages use network byte order, distinct IPv4/IPv6
  template IDs (256/257), zero Set padding, and sequence numbers counting
  **Data Records**, not templates or datagrams. Both templates are sent in a
  separate message at startup and after socket recreation, before dependent
  data. Data messages use the collector's cached templates; they no longer
  repeat templates in every message. Refresh occurs when `template_refresh`
  elapses or `template_refresh_packets` successful data messages have been
  sent, whichever occurs first, including during continuous traffic.
  Refresh is checked on polling and between data batches; idle refresh still
  continues. The collector's UDP template lifetime should be at least three
  times `template_refresh`. No Template Withdrawals are sent over UDP.
- Due records are batched by complete serialized byte size, with exact zero
  padding, rather than sent one per datagram. A message may contain both IPv4
  and IPv6 Data Sets; records are never split or reordered. Each successful
  datagram acknowledges only its own records;
  partial batch failures retain the remaining records without replaying
  already accepted data.
- The selected UDP transport follows RFC 7011's UDP wire requirements.
  This is **not** a claim of full RFC 7011 transport
  implementation compliance: section 10.1 also mandates SCTP support, which
  this UDP-only feature does not implement.
- UDP is not reliable or encrypted. A successful send is not a collector
  acknowledgment; use a trusted, provisioned management network and tolerate
  loss/reordering. Sequence gaps let collectors detect lost datagrams.
  Collectors must cache templates per transport session and observation
  domain. A lost template message can leave data undecodable until refresh.
  Socket failures retain unsent deltas for retry; process death loses
  unexported in-memory state.
- Export failures close the failed socket and retry with exponential backoff
  from 1 second up to 30 seconds. Metering and bounded buffering continue
  during an outage. Socket recreation preserves flow/pending state and the
  observation domain; a new UDP session starts with a fresh sequence number.
  `EMSGSIZE` errors reduce the message budget to the reported PMTU payload,
  or halve it if no smaller usable estimate is available, and retain the
  unsent suffix for rebatching after backoff. PMTU query failures remain
  explicit errors. The smaller limit survives reconnection, even if the new
  socket starts with a larger estimate. If the budget cannot carry templates,
  no oversized template is repeatedly sent; retries can recover when the
  kernel subsequently reports a larger PMTU. Otherwise restart to increase
  a previously reduced limit.
  Shutdown detaches capture first and uses a two-second overall flush budget,
  not a timeout per flow. Unexported leftovers are explicit errors. Only error
  logs are provided; no status API, status file, or periodic summaries exist.

### Metering limits

Every supported IP packet selected by the sampler is observed. With the
default `sampling_rate=1`, no sampling is performed.
Counters describe Linux **skbs** and IP-layer octets, excluding
Ethernet/VLAN headers. GRO/GSO/TSO can coalesce skbs, so these are not guaranteed
wire-packet counts with offloads enabled. For wire-packet accounting, disable
coalescing/segmentation offloads on both ends separately; the daemon deliberately
does not alter offload settings or the rename operation.

IPv6 extension chains (up to eight headers) and up to two VLAN headers are
parsed. IPv4 fragments (including first fragments) and all IPv6 Fragment
headers (including atomic fragments) are excluded rather than guessed into
portless flows. Sampled exclusions are counted separately in the kernel;
they are intentional exclusions, not capacity/malformed errors. IPv6 jumbo
payloads, longer extension chains and malformed IP headers increment explicit
unsupported/malformed counters. Non-IP traffic is outside this feature's scope.

`max_flows_per_veth` (1..1048576) now bounds **concurrently retained directional
flows**, not lifetime keys. Idle/active expiration reclaims entries; when full,
the least recently updated flow is queued for early export with
`flowEndReason=5` (lack of resources). The collector aggregates multiple
segments of a connection.

To avoid races between kernel updates and flow deletion, eBPF transfers
sampled packet-header metadata through a bounded **4 MiB ring buffer per veth**.
The daemon owns the flow table and its expiration/LRU indexes; no concurrently
updated kernel flow entry is deleted or reset. This introduces per-sample
userspace processing and is not a guarantee of wire-rate performance.
The ring holds roughly 65,536 metadata events and absorbs bursts, not
sustained overload. Polling and Docker reconciliation delays can fill it;
ring-buffer losses are counted and reported explicitly.

Each veth also has a bounded pending-record queue of
`max(4096, max_flows_per_veth)` records. During prolonged collector failure,
completed records are retained until this queue fills. Existing active
flows can still accumulate counts, but a new key requiring eviction is then
rejected and logged with an exact packet-loss count rather than discarding
already pending records. Memory and throughput are finite; there is no claim
of unlimited-capacity or lossless monitoring.

When upgrading from an earlier template schema, restart the exporter
and ensure the collector accepts the refreshed templates. Template IDs stay
256/257, but fields have changed. The first 13 fields follow the provided
unidirectional IPFIX schema; three additional standard IEs retain per-veth
identity and end reason. Records are 77 bytes (IPv4) or 101 bytes (IPv6),
not the reference schema's 52/76 bytes. Collectors must decode templates,
not hard-code record widths.

## Optional eBPF firewall and Sando ignore-list import

Firewalling is **disabled by default**. Add
`--firewall-config=/etc/veth-manager/firewall.yaml` alongside `--ipfix-config`.
Only `enabled: true` activates enforcement; an omitted or false `enabled`
leaves the exporter observational and does not open the drop log or load rules.
Enabled firewalling requires daemon mode and IPFIX configuration. The same
verified managed host-veth scope and Linux/capability requirements apply.

```yaml
enabled: false
audit_mode: false
enforcement_direction: both
permit_all_icmp: true
manual_rules: manual.yaml
sando_rules: sando.yaml
sando_endpoint: "http://192.0.2.10:8000"
sando_direction: both
sando_assignment: endpoint
sando_poll: false
sando_poll_interval: 60s
drop_log: /var/log/veth-manager/firewall-drops.log
```

Rule/log paths are relative to this configuration file unless absolute.
`enabled` defaults to false, `permit_all_icmp` defaults to true (IPv4 ICMP and
ICMPv6), and `sando_direction` defaults to `both`. The three paths are required
when enabled. Unknown/duplicate keys, nulls, aliases, invalid rules and multiple
YAML documents are errors. Restart the daemon to load configuration changes.
Policy changes also require restart unless Sando polling is enabled below;
a one-shot sync command does not directly notify a running daemon.

### Enforce one direction, observe both

`enforcement_direction` defaults to `both` and accepts `ingress`, `egress`,
or `both`. With `enabled: true` and `audit_mode: false`:

| Setting | Container -> network (ingress) | Network -> container (egress) |
| --- | --- | --- |
| `ingress` | Enforce policy | Permit all; log policy denies as `WOULD_DROP` |
| `egress` | Permit all; log policy denies as `WOULD_DROP` | Enforce policy |
| `both` | Enforce policy | Enforce policy |

Both hooks always evaluate the policy and keep the same IPFIX capture,
sampling and filtering behavior. This is not a public-Internet-only filter:
container traffic to LAN/private IPs is also ingress. For example:

```yaml
enabled: true
audit_mode: false
enforcement_direction: ingress
```

The other direction remains policy-observed, including malformed/unsupported
packet logging, but this daemon never drops it. Only policy-permitted
TCP/UDP traffic creates state, including policy allows in the non-enforced
direction. A packet allowed solely by directional bypass does not create
reply state that could bypass policy in the enforced direction.
`audit_mode: true` overrides this scope and permits both directions.
`enabled: false` disables policy evaluation/logging altogether.

This global enforcement setting is separate from individual rule `direction`
and `sando_direction`: those decide where a rule matches, not whether the
firewall may drop packets at a hook. Restart to change the enforcement scope.

### Audit mode (permit all, log would-be drops)

Set `enabled: true` and `audit_mode: true` to evaluate the configured policy
without dropping traffic. `audit_mode` defaults to **false**; an omitted or
false setting preserves enforcement when the firewall is enabled.
`enabled: false` disables both enforcement and auditing, regardless of
`audit_mode`. Restart the service after changing either setting:

```yaml
enabled: true
audit_mode: true
```

Audit mode uses the same rule order, system allowances, per-veth limits and
TCP/UDP tracking. Only traffic permitted by the policy creates connection
state; packets allowed solely because of audit mode do not create state.
Would-be drops (including malformed/unsupported packets and state-table
capacity failures) are logged as `WOULD_DROP` instead of `DROP` to the same
`drop_log` file. Logging-buffer overflow never blocks traffic in audit mode.
IPFIX capture still precedes the verdict and retains its sampling/exclusion
semantics. Existing external firewalls can still drop traffic.

Audit requires the same valid policy, IPFIX configuration, capabilities,
successful attachment and writable log as enforcement. Startup logs include
`audit_mode` so operators can distinguish the modes. Set `audit_mode: false`
and restart to enforce the policy; restart clears connection state, so audit
results are a preview, not a guarantee of identical future traffic.

### Analyze firewall logs (read-only)

```bash
# Default: last 24 hours, ranked by logged packet count.
sudo veth-manager --propose-rules --firewall-config=/etc/veth-manager/firewall.yaml
# Last seven days, ranked by bytes; no configuration or Docker access needed.
sudo veth-manager --propose-rules --firewall-log=/var/log/veth-manager/firewall-drops.log --since=7d --sort=bytes
```

`--since` accepts positive Go durations (e.g. `12h`, `168h`) or whole days
(`7d`, where one day is 24 hours). `--sort` accepts `packets` (default) or
`bytes`. An explicit `--firewall-log` overrides the configured `drop_log`.
The command prints a table and exits without loading policy files, attaching
eBPF, changing rules, or contacting Docker/Sando. Despite its name,
`--propose-rules` provides review evidence, not generated/applied allow rules.

Before aggregation, TCP/UDP records with a source port lower than the destination
port have their source/destination IPs swapped, destination port set to the
original source port, and ingress/egress direction reversed. This groups replies
under the lower service port rather than ephemeral client ports, even when only
return traffic is logged. Equal-port records are unchanged. This port-order
heuristic is not connection tracking and can misidentify unusual client/service
port assignments.

Rows aggregate normalized source IP, destination IP, destination port and numeric
IP protocol, ignoring ephemeral client ports. Interface, normalized direction,
verdict, rule ID and reason remain separate so independent rule matches and actual
versus would-be drops are not conflated. `PACKETS` counts matching log records;
`BYTES` sums `LEN`. `LAST_SEEN` is the age of the newest matching log record within
the selected window, displayed in whole minutes (for example, `2 minutes ago`),
regardless of log record order or timezone. Records less than a minute old show
`0 minutes ago`. Ages use the same reference time as the lookback window.
ICMP's destination field is `(type << 8) | code`.
No matching records produces only the table header. Malformed records and
read errors fail explicitly rather than silently producing incomplete results.

Only the selected file is read; rotated/archived logs are not discovered
automatically. Counts cover logged `DROP`/`WOULD_DROP` events only, not allowed
traffic, live kernel rule counters, or events lost to logging-buffer overflow.
New records append an RFC3339 `TIME` with year/timezone. Older syslog-style
records are supported using the analyzer's local timezone and current year
(previous year for months later than the current month); their original year,
timezone and DST ambiguity cannot be recovered. Use timestamped records for
reliable historical analysis.

### Order, direction and state

The firewall evaluates **every packet**, independent of IPFIX sampling or
capture filters:

1. ARP for link reachability, then valid ICMP/ICMPv6 if `permit_all_icmp`.
2. Existing permitted TCP/UDP flow state and its replies.
3. Manual rules in file order, first match wins.
4. Imported Sando allow rules in file order.
5. Default deny.

Directions are **host-veth-relative**: `ingress` is container -> network;
`egress` is network -> container; `both` matches either hook. Directions apply
to each manual rule and to imported rules via `sando_direction` at sync time.
System allowances precede manual denies: to control ICMP manually, set
`permit_all_icmp: false`. State is per veth and keyed by addresses, ports,
protocol, IP family and hook direction, not TOS. UDP state is created by an
allowed datagram. TCP reply state requires an allowed SYN, a reverse SYN/ACK
and a forward ACK before ordinary reverse data is allowed; explicit policy
can still allow packets independently of state.

Idle state expires after 5 minutes for TCP and 30 seconds for UDP. TCP FIN
reduces the idle timeout to 30 seconds; RST invalidates state. Polling reclaims
expired entries. The non-LRU state table holds 65,536 entries per veth;
packets requiring new state are dropped and logged if it is full. Established
entries are not evicted to make room. This is bounded tuple/handshake tracking,
not full Linux conntrack: it does not validate TCP sequence windows, perform
NAT, or recognize related ICMP errors when the global ICMP allowance is off.
State is lost on detach, interface replacement or process restart.
DHCP, other broadcast traffic and multicast are not automatically allowed;
add explicit rules where needed. Reply state requires the exact reversed
tuple and does not associate a broadcast request with a unicast reply.

### Manual and imported rules

Both files contain a `rules` list; initialize each with `rules: []`.
For example, a manual file:

```yaml
rules:
  - id: block-container-dns
    direction: ingress
    action: deny
    source: "*"
    destination: "*"
    protocol: udp
    destination_port: "53"
  - id: allow-container-https
    direction: ingress
    action: allow
    source: "10.0.0.0/24"
    destination: "*"
    protocol: tcp
    destination_port: "443"
```

`id`, `direction`, `action`, `source`, `destination`, and `protocol` are
required. Addresses accept literal IPv4/IPv6, CIDRs, or `"*"`. Protocols accept
`tcp`, `udp`, `icmp`, `icmp6`, `sctp`, a decimal number 0..255, or `"*"`.
Optional `source_port` / `destination_port` accept decimal 0..65535 or `"*"`;
optional `log: false` suppresses DROP/WOULD_DROP events for a matched deny rule.
Omitting `log` or setting `log: true` preserves logging. The value must be a
YAML boolean. This does not change enforcement, IPFIX capture, default-deny
logging, or malformed/state-capacity errors, and does not introduce allow
logging. For example, a manual rule can silently deny UDP port 5353:

```yaml
rules:
  - id: quiet-mdns
    direction: both
    action: deny
    source: "*"
    destination: "*"
    protocol: udp
    destination_port: "5353"
    log: false
```

For port constraints,
omission means unconstrained. Zero remains a literal constraint. ICMP uses
the exporter's zero source port and `(type << 8) | code` destination field,
not transport ports. Rules must have unique IDs across both files. There is
no global 256-rule limit on the saved policy: the limit is **256 applicable
rules per veth**, including its manual and imported rules. Manual matching
preserves packet source/destination orientation.

All manual rules remain global and count toward every veth. At attachment,
`sando_assignment` controls which imported rules are selected:

- `endpoint` (new default): select when either **non-wildcard** address
  constraint matches the container's Docker-reported IPv4/IPv6 address.
  `.18 -> *` and `* -> .18` are assigned to `.18`'s veth, not every veth.
  `10.4.20.0/24 -> *` is assigned to containers in that subnet.
  `* -> *` remains global. A `/0` is treated as a wildcard for assignment.
- `all_matches`: previous behavior, where a wildcard on either side selects
  every veth. Set this explicitly to preserve the old assignment behavior.

**Upgrade note:** the new default narrows where one-sided wildcard rules
are installed. It does not narrow packet matching inside a selected veth:
`*` still means any IP, including Internet/LAN destinations, and CIDRs still
match whole subnets. Selection preserves bidirectional matching, per-rule
direction, ordering and original rule IDs. Applicable rules count toward
each selected veth; the limit is never implemented by truncating the policy.

When Docker reports an address change, reconciliation atomically replaces
that veth's rule table without detaching either hook. TCP/UDP state is retained
unless Sando polling is enabled and the effective rules change. Each packet
sees one complete old or new table, not partially updated
rules. A failed or over-capacity
refresh reports the veth name/count and retains its previous table.
An over-capacity new veth is not attached and remains unprotected; other
veths can still be reconciled.

Missing or unspecified Docker addresses retain all Sando rules for that IP
family rather than guessing which rules are irrelevant. IPv6 link-local
constraints are also conservatively retained for automatic addressing;
`all_matches` additionally retains unspecified-address constraints.
Consequently an endpoint without reliable Docker
address metadata may still exceed its per-veth limit. Assignment is intended
for ordinary container endpoints, not containers routing other hosts' IPs.

Sando sync uses [mayberryjp/sando's ignore-list API](https://github.com/mayberryjp/sando/blob/main/src/routers/ignorelist.py):
`GET <sando_endpoint>/api/ignorelist`. It accepts the JSON array of
`ignorelist_id`, `src_ip`, `dst_ip`, `dst_port`, and `protocol`, including
numeric strings and `"*"` constraints. IDs are prefixed with `sando:`.
Generated rules are allow-only with `sando_match: true`, preserving Sando's
bidirectional address matching and port matching against **either** endpoint,
within the configured hook direction. Manual rules cannot use `sando_match`.
Sando's `added` metadata is ignored. The currently published API has no
authentication requirement; this client does not implement credentials.
Use HTTPS or a trusted management network; redirects are rejected.

```bash
# Fetch, validate against manual rules, atomically replace only sando.yaml,
# print the generated YAML, and exit. Works while firewalling is disabled.
sudo veth-manager --firewall-config=/etc/veth-manager/firewall.yaml --sando-sync
# Override sando_direction for this sync only (does not edit firewall.yaml).
sudo veth-manager --firewall-config=/etc/veth-manager/firewall.yaml --sando-sync --sando-direction=ingress
# Print the saved generated policy without contacting Sando.
sudo veth-manager --firewall-config=/etc/veth-manager/firewall.yaml --sando-dump
```

Without polling, sync is explicit only: no startup or periodic downloads. HTTP errors, invalid
responses, duplicate IDs, responses exceeding 4 MiB and file-write failures exit
nonzero and preserve the last saved policy. A successful empty list removes
all imported rules, not manual rules. Both rule files must exist before
enabling the firewall; missing policy files are startup errors.
Sync validates and saves the whole policy; the per-veth limit is checked by
the daemon when attaching or refreshing each endpoint, not during sync.
`--sando-direction` accepts only `ingress`, `egress`, or `both` and requires
`--sando-sync`; it cannot be used with `--sando-dump`. It changes the direction
written into generated rules, not the global enforcement scope. Assignment
is done by the daemon using `sando_assignment` at attachment, since sync has
no Docker endpoint inventory; the saved Sando file still contains all rules.
Without polling, restart after syncing rules. Changing assignment mode always
requires restart.

### Automatic Sando updates (opt-in)

```yaml
sando_poll: true
sando_poll_interval: 60s
```

Polling defaults to **disabled**. The interval defaults to **60s**, accepts Go
durations such as `30s` or `2m`, and must be at least `1s`. Polling requires an
enabled firewall and `sando_endpoint`. Both saved rule files must still exist
at startup; the daemon starts with those rules, then fetches immediately and
polls again at the configured interval after each result. Requests have a
30-second timeout, are cancellable on shutdown, and run in a separate worker
so slow Sando responses do not block Docker reconciliation or IPFIX polling.

No Sando change is required: `GET /api/ignorelist` responses are validated and
compared with each veth's effective rules. When Sando supplies an `ETag`, the
next request includes `If-None-Match`; Sando may return **304 Not Modified**
with no body. Otherwise it returns **200** with the existing JSON array and a
new ETag. The validator must represent the complete list, including additions,
edits, deletions and an empty list, and must change whenever that representation
changes. A content hash of a consistently ordered response is suitable. The
body and ETag must describe the same snapshot. No separate version endpoint
or push notification is needed. Without ETags, ordinary downloads continue
to work; unchanged policies are neither rewritten nor reinstalled.

The daemon validates the combined manual/imported policy and every attached
veth's capacity before atomically saving the generated file. It then replaces
only changed veth tables without detaching hooks or restarting. Existing
TCP/UDP state is revoked on those veths, including for previously permitted
connections; veths with unchanged effective rules retain their tables and
state even when another endpoint's rule deletion shifts global rule indices.
This invalidates firewall permissions, not operating-system sockets or TCP
sessions directly. Manual rules retain precedence and are never overwritten.

HTTP/validation/capacity/save failures are logged and leave active policies
unchanged. A kernel application failure is logged per veth: that veth retains
its old table, successful veth updates remain active, and subsequent polls
retry failed applications (including on 304). The saved file can therefore be
ahead of a failed veth's active policy until retry succeeds. A successful empty
list removes imported rules and revokes state only on affected veths.
Policy swaps are atomic **per veth**, not across all veths simultaneously.
Drop events retain their generation: very late events from older tables are
identified by generation/rule index rather than attributed to a different rule.
This is not a general configuration/file watcher: restart after editing
firewall configuration or manual rules; do not rely on local generated-file
edits, which polling replaces with Sando's policy.

### Capture, drops and deployment

The same hook submits eligible IPFIX metadata **before** returning a firewall
drop, so denied flows remain visible. Existing IPFIX sampling, capture filters,
fragment exclusions, collector exclusions and bounded-buffer losses still
apply; use `sampling_rate: 1` and `capture_filter: ""` for fullest visibility.
Fragments remain excluded from IPFIX flows, but the firewall evaluates the
first IPv4/IPv6 fragment using its transport header and normal rules or
connection state. Only an allowed first fragment authorizes later fragments
with the same source, destination, IP protocol, fragment ID and hook direction
on that veth. Authorization lasts 30 seconds from the first fragment, is not
extended by later fragments, and is invalidated by policy-generation changes.
IPv6 atomic fragments are evaluated normally without creating authorization.
Orphaned, out-of-order (before the first), expired and unauthorized fragments
are denied with `unsupported-or-fragment`; a full 65,536-entry per-veth
fragment table denies new authorizations with `state-table-full`. Expired
entries are reclaimed by the existing state cleanup. Audit mode and
unenforced hooks still log would-be denials and do not authorize denied
datagrams. This is fragment authorization, not packet reassembly or overlap
detection. Malformed/unsupported packets and non-IP frames other than ARP
are denied and logged.
A full IPFIX buffer never bypasses firewall enforcement. This order applies
within this daemon's hook; it cannot observe packets dropped by an earlier
external classifier.

Drop events are independent of IPFIX sampling and written to `drop_log` in
syslog-style human-readable lines, not JSON:

```text
Nov 23 12:30:00 host veth-manager[1234]: DROP IF="veth_app" DIR=ingress SRC=10.0.0.1 DST=203.0.113.1 PROTO=6 SPT=42000 DPT=80 LEN=40 RULE="default" REASON=default-deny TIME=2026-11-23T12:30:00Z
```

Each line includes packet time, interface, direction, tuple, length, matched
rule and reason. Rule/interface strings are escaped to prevent multiline
records. Logs use a separate 1 MiB ring per veth; overflow and write failures
are explicit daemon errors, never permission to pass traffic. The daemon
opens the log in append mode (new files 0600). Parent directories
must exist; the supplied systemd drop-in creates the default log directory.

Debian packages install `/etc/logrotate.d/veth-manager` and depend on `logrotate`.
The default `/var/log/veth-manager/firewall-drops.log` rotates daily or when it
exceeds 10 MiB at a logrotate check, retaining 7 compressed archives. Missing or
empty logs are skipped. `copytruncate` keeps the daemon writing without a restart,
but records written between copying and truncating can be lost. The size trigger
is not a strict cap: logs can exceed 10 MiB between checks, and daily rotation is
performed only when logrotate runs. Verify the host's logrotate timer or cron job
is enabled; more frequent checks are needed for a busy log.

For source/archive installs, install the supplied configuration:

```bash
sudo install -m 0644 deploy/veth-manager.logrotate /etc/logrotate.d/veth-manager
sudo logrotate --debug /etc/logrotate.d/veth-manager
```

If `drop_log` uses a custom path, update the logrotate entry to match. Rotation
does not change `--propose-rules`: it reads only the selected active log, not
archives, so its available history shrinks after rotation.

Install the four supplied firewall examples from `/usr/share/doc/veth-manager/`
(or `deploy/` for source installs): `firewall.example.yaml` as
`/etc/veth-manager/firewall.yaml`, `manual.example.yaml` as `manual.yaml`,
`sando.example.yaml` as `sando.yaml`, and `firewall.conf` as
`/etc/systemd/system/veth-manager.service.d/firewall.conf`. **Replace/remove the
previous `ipfix.conf` drop-in** rather than having competing `ExecStart`
overrides. Set your collector and Sando endpoint, sync and inspect the policy,
then explicitly set `enabled: true`, run `systemctl daemon-reload`, and restart.
Preserve custom naming flags. For a custom log directory, also configure
systemd `ReadWritePaths`; the base unit uses `ProtectSystem=strict`.

Enforcement begins only after successful attachment. Discovery/attachment
gaps, daemon downtime and shutdown detach leave traffic unprotected; this
feature is not a fail-closed container lifecycle controller. Attachment
failures are explicit errors and are retried. Verifier/rule/log-open failures
abort startup before reconciliation. Keep an independent host firewall if
protection must survive daemon failure.

## Controlled primary-name repair

With `renaming.enabled: true`, the default `--mode=primary-repair` gives the verified live host veth the
requested **kernel primary name**, not just an alternative name or vnStat
alias. For example, traffic for the labeled container flows through
`veth_youtube`, and `vnstat -i veth_youtube --days` selects that live interface.

**This mode briefly interrupts each affected container's networking.** Linux
usually rejects renaming an UP link, so the daemon lowers only that verified
host veth, renames it, restores UP, and checks the endpoint, ifindex, MAC,
bridge, peer, primary name, and administrative state. Ordinary failures and
cancellation trigger rollback with a separate bounded recovery context.
A later scan also brings the verified managed peer UP if an abrupt process
death left it DOWN. An interruption cannot be made atomic against SIGKILL,
host failure, or a concurrent external network manager.

If a disconnected DHCP pair occupies the requested name, reclamation requires
both reciprocal ends to be visible in the host namespace, the bridge to match,
the detached peer to be administratively DOWN with the Docker endpoint MAC,
and its name to match the plugin's `<12 hex digits>-dh` convention. The bridge
end must be disconnected. The daemon restores that stale bridge end's original
`dh-<12 hex digits>` name rather than deleting the pair, then names the live
peer. It rechecks identity before mutations and rejects live or ambiguous
collisions, occupied restoration names, duplicate labels, and endpoint races.
Shared bridges, VLANs, physical NICs, and container-side links are never lowered
by this repair. New or recreated labeled endpoints are reconciled automatically.

Deploy the matching binary and systemd unit together. The packaged unit uses
`--mode=primary-repair`; remove old mode overrides if you want the new default.
Explicitly enable renaming in `manager.yaml`; the mode alone does not opt in.
The `--dry-run` flag reports planned repairs without changing links or vnStat.
Successful repair retains the interface index, MAC, and bridge. The daemon
does not configure addresses or routes.
vnStat registration and aliases are reconciled using the resulting primary
name; historical rows are not deleted or merged. DHCP plugins may still leave
detached pairs during teardown after a primary rename; subsequent scans handle
proven stale-name collisions using the same rules instead of deleting links.
The separate opt-in orphan cleanup can remove eligible detached `dh-*` pairs
after its observation grace period.

## Non-disruptive naming and monitoring

The optional `--mode=auto` never attempts a primary rename. It adds an
alternative name when available and synchronizes vnStat using the actual
kernel primary name, not the alternative name. On every scan it detects
missing registrations or incorrect aliases and repairs them, including after
container recreation. Reads are batched and writes are verified afterward.

If an old interface occupies a requested name (for example a disconnected
`veth_symbols` while the container uses `dh-90941d7c6428`), auto mode records
a `naming_warning` and monitors the verified live `dh-*` interface with the
vnStat alias `veth_symbols`. It does not delete, rename, or take down the
conflicting link. Alternative-name failures also fall back to monitoring.
Duplicate label claims and endpoint validation failures still fail closed.
Without vnStat installed, name conflicts remain errors rather than being
reported as repaired.

Deploy the new binary **and** updated systemd unit, then reload systemd and
restart `veth-manager.service`. The packaged unit starts after vnStat.
Explicit primary/alternative modes retain strict naming behavior; primary
mode does not take links down and can still fail on an UP device.
vnStat must already be installed; vnstatd initializes and collects into its
database. Startup/database failures remain visible and are retried.
Use `--vnstat=off` to disable integration or `--vnstat=/path/to/vnstat` to
require a particular executable. Container-namespace macvlan interfaces are
not registered in the host's vnStat database.

Traffic history is never removed, renamed, or merged. Old rows remain for
historical reporting; aliases do not combine history across recreated
interfaces or change the kernel name used with `vnstat -i`. This repairs
ongoing monitoring in auto mode without interrupting container networking, rather than
attempting unsafe live renames or guessing which old links can be deleted.
The unit permits writes to `/var/lib/vnstat` and includes `CAP_DAC_OVERRIDE`
to update a database owned by the vnstat account. Sites using a custom
database directory must add that directory to the unit's `ReadWritePaths`.

## Choosing a naming mode

**Do not assume an active veth can be renamed.** Linux normally rejects primary
interface renames while a device is UP (`Device or resource busy`). Strict
`--mode=primary` never brings interfaces down or retries with downtime.
Consequently, **primary mode will commonly report errors instead of renaming
running Docker interfaces**. Primary mode is an explicit opt-in, not a
guarantee of live renaming.

For readable names without disrupting running containers, use
`--mode=alternative`. This adds a Linux alternative name while preserving
Docker's original primary name and interface index. Alternative names require
kernel support (Linux 5.4+); names are managed directly through rtnetlink.
Ubuntu 22.04/24.04
normally have that support. Whether other software accepts alternative names
depends on the tool/version: test your actual tcpdump, SNMP agent, and monitoring
stack. An alternative name does **not** change the primary name shown by all
tools.

Use the default primary-repair mode only when its controlled interruption is
acceptable. For zero-interruption requirements, choose auto or alternative
mode and do not expect `vnstat -i` to resolve an alternative name or alias.

## Macvlan and docker-net-dhcp networks

`ghcr.io/claymore666/docker-net-dhcp:<tag>` networks configured with
`mode=bridge` (also the plugin's default when `mode` is omitted) are supported.
The network must specify its host bridge with the `bridge` option, for example
`{"bridge":"br-vlan20","mode":"bridge"}`. These endpoints use host-side veth
naming with the same reciprocal-peer and bridge-membership checks as Docker's
built-in bridge driver. The shared bridge itself is never renamed.

Native Docker `macvlan` networks and
`ghcr.io/claymore666/docker-net-dhcp:<tag>` networks configured with
`mode=macvlan` are supported. The network must specify its host `parent`
interface, for example `enp44s0.20`.

Macvlan has no host-side veth peer. For these networks, the same labels and
`--mode` setting apply to the MAC-matched macvlan interface **inside the
container's network namespace**, not to the shared host parent. The daemon
validates the kernel link type and parent index before changing anything.
It never renames the host VLAN or physical interface. Primary-repair mode uses
alternative naming for macvlan; it does not interrupt container-side links.

Primary mode attempts a primary rename without bringing the interface down.
An UP macvlan interface can reject that rename, just like an UP veth; this is
reported as an error, not a skipped endpoint. Use `--mode=alternative` to add
the requested name while retaining `eth0` (or the existing primary name).
Neither mode creates a host-visible per-container interface. Applications or
DHCP clients that depend on the original interface name may be affected by a
successful primary rename; alternative mode avoids changing that name.

## Labels

One bridge-network attachment:

```yaml
services:
  pihole:
    image: pihole/pihole:latest
    labels:
      host-veth-name: veth_pihole
```

Names must be 1-15 ASCII characters using letters, digits, `_`, `-`, or `.`.
`.` and `..` are not allowed. Empty or invalid labels are errors, not silently
ignored. Keep names unique across the entire host, including alternative names.
Compose replicas need distinct labels per container or they will conflict.
Labels belong on the **container**, not the Docker network.

Multiple network attachments:

```yaml
services:
  app:
    image: nginx:alpine
    labels:
      host-veth-name.frontend: veth_app_front
      host-veth-name.backend: veth_app_back
    networks:
      - frontend
      - backend
networks:
  frontend:
    name: frontend
  backend:
    name: backend
```

The suffix must match the **actual Docker network name**, not necessarily the
Compose key. Without `name: frontend`, Compose usually names it
`<project>_frontend`, and the label must be
`host-veth-name.<project>_frontend`.

Network-specific labels override `host-veth-name`; they can also be used alone,
leaving other networks unmanaged. With a base label and multiple network
attachments, names are `<first-8-base-characters>-<6-hex-network-name-hash>`.
This stays within 15 bytes and is stable across recreation of a network with the
same name. Hash collisions are possible and are reported, never silently
resolved. Adding/removing attachments can change base-derived names; use
network-specific labels when attachment-independent names matter.

Existing names on an interface are not removed when labels change or the daemon
stops. In alternative mode, old alternative names can therefore accumulate until
Docker deletes the endpoint. Remove obsolete names explicitly, after checking
their ownership:

```bash
sudo ip link property del dev veth_original altname veth_old
```

## Migrating from veth-rename / veth-renamer

The old repository was named `veth-rename`, but its released Debian package,
binary and service were named **`veth-renamer`**. The new package is
**`veth-manager`**. Do not run both daemons together. Schedule a maintenance
window: stopping/restarting clears firewall connection state and starting in
`primary-repair` mode can briefly interrupt managed interfaces.

1. Back up the old configuration and unit overrides before removing anything:

   ```bash
   backup="$HOME/veth-renamer-backup-$(date +%Y%m%d-%H%M%S)"
   mkdir -m 0700 "$backup"
   for path in /etc/veth-renamer /etc/systemd/system/veth-renamer.service \
       /etc/systemd/system/veth-renamer.service.d /etc/logrotate.d/veth-renamer; do
       if sudo test -e "$path"; then
           sudo cp -a -- "$path" "$backup/"
       fi
   done
   dpkg-query -W -f='${Package} ${Status} ${Version}\n' veth-renamer
   ```

2. Stop the old service and remove the old package (not purge; keep configuration):

   ```bash
   sudo systemctl disable --now veth-renamer.service
   sudo apt-get remove veth-renamer
   ```

   If your host actually has a package named `veth-rename`, remove that package
   instead. For an unpackaged/manual installation, stop and disable its old unit
   and remove the backed-up `/etc/systemd/system/veth-renamer.service` explicitly.
   Old binaries in `/usr/local/sbin` are not removed by apt; remove them separately
   after verifying they are no longer used. Existing interface names are not
   restored when the old service stops.

3. Install the new package **without `--start`** until configuration is migrated:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/mayberryjp/veth-manager/main/install.sh | bash
   ```

   This requires a published release in the new repository. The package conflicts
   with both old package names to prevent simultaneous package installations.

4. Copy your configuration and drop-ins to their new locations. The following
   commands preserve existing configuration files and refuse to overwrite
   pre-existing override directories:

   ```bash
   if sudo test -d /etc/veth-renamer; then
       sudo install -d -m 0755 /etc/veth-manager
       sudo cp -an /etc/veth-renamer/. /etc/veth-manager/
   fi
   if sudo test -d /etc/systemd/system/veth-renamer.service.d; then
       sudo test ! -e /etc/systemd/system/veth-manager.service.d &&
           sudo cp -a /etc/systemd/system/veth-renamer.service.d /etc/systemd/system/veth-manager.service.d
   fi
   ```

   Edit the copied YAML and systemd `.conf` files, replacing old binary/service
   and path references:

   | Old | New |
   | --- | --- |
   | `/usr/sbin/veth-renamer` | `/usr/sbin/veth-manager` |
   | `/etc/veth-renamer` | `/etc/veth-manager` |
   | `/run/veth-renamer` | `/run/veth-manager` |
   | `/var/log/veth-renamer` | `/var/log/veth-manager` |
   | `veth-renamer.service` | `veth-manager.service` |
   | `LogsDirectory=veth-renamer` | `LogsDirectory=veth-manager` |
   | `RuntimeDirectory=veth-renamer` | `RuntimeDirectory=veth-manager` |

   Relative `manual_rules: manual.yaml` and `sando_rules: sando.yaml` need no
   changes. Preserve custom options, collectors and Sando endpoints. Update
   `/etc/veth-manager/manager.yaml` to explicitly enable renaming if desired;
   the packaged configuration leaves renaming and cleanup disabled.
   `ReadWritePaths` in any Sando-write override to `/etc/veth-manager`.
   Install the new supplied IPFIX/firewall drop-in if you did not have one before.
   Custom drop-log paths also require updating `/etc/logrotate.d/veth-manager`.
   Do not copy an old full unit over the new packaged unit; migrate its overrides.
   Old logs remain in the old log directory. The new analyzer can read them with
   `--firewall-log=/var/log/veth-renamer/firewall-drops.log`.

5. Review the effective unit and start only the new service:

   ```bash
   sudo systemctl daemon-reload
   sudo systemctl cat veth-manager.service
   sudo systemctl enable --now veth-manager.service
   sudo systemctl is-active veth-manager.service
   sudo journalctl -u veth-manager.service --since "5 minutes ago" --no-pager -o cat
   dpkg-query -W -f='${Version}\n' veth-manager
   ```

   Keep backups until the new service, monitoring and firewall are verified.
   Update operational scripts, dashboards and journal filters to the new name.

### Upgrade and uninstall

Rerun the installer to upgrade. Existing systemd overrides are preserved.
Without `--start`, a running daemon keeps its old process until you explicitly
run `sudo systemctl restart veth-manager.service`.

```bash
sudo apt-get remove veth-manager
```

Removal stops and disables the service. Administrator-created overrides are
left alone. Neither removal nor stopping the service restores primary names or
deletes alternative names on existing interfaces.

The package installs the binary in `/usr/sbin` and the unit in
`/usr/lib/systemd/system`. If migrating from an older manual install in
`/etc/systemd/system/veth-manager.service`, first back up that local unit and
migrate custom settings into a systemd drop-in. Stop the service and remove the
old local unit after backing it up, then run `sudo systemctl daemon-reload` and
retry the installer. It refuses to silently override a local unit. An old
`/usr/local/sbin/veth-manager` binary can be removed separately after migration.

## Build and preview

Requires Go 1.23+ to build; Go downloads cilium/ebpf, netlink, netns, x/sys and yaml.v3.
The precompiled little-endian eBPF object is embedded, so ordinary builds and
cross-builds require neither clang nor runtime compilation.
Run these commands on the Ubuntu host:

```bash
go test ./...
go vet ./...
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -o bin/veth-manager .

# Enable renaming in deploy/manager.yaml before testing naming.
# Keep orphan_cleanup.enabled false for these one-shot commands.
# Read-only preview: no interface changes.
sudo ./bin/veth-manager --manager-config=deploy/manager.yaml --once --dry-run

# Repair actual primary names, briefly interrupting the affected host veth.
sudo ./bin/veth-manager --manager-config=deploy/manager.yaml --once

# Non-disruptive alternative naming.
sudo ./bin/veth-manager --manager-config=deploy/manager.yaml --once --mode=alternative
```

After changing the packet parser, regenerate the checked-in object on Linux
amd64 using clang 14 and libbpf headers:

```bash
sudo apt-get install -y clang-14 libbpf-dev
go generate ./internal/flow
go test ./...
# Kernel tests create veths only inside this isolated network namespace.
go test -c -o /tmp/veth-flow.test ./internal/flow
sudo unshare --net env FLOW_INTEGRATION=1 /tmp/veth-flow.test -test.run '^TestKernel' -test.v
rm /tmp/veth-flow.test
```

For cross-compilation, set `GOOS=linux GOARCH=amd64` (or `arm64`) when
building. The executable runs only on Linux and must be in the host network
namespace. Interface discovery, address checks, renaming, state changes,
alternative names, and deletion use native rtnetlink. Container namespace
access uses `setns` on a dedicated locked OS thread only to create a netlink
socket; the original namespace is restored before any queries or mutations.
The daemon requires neither `ip` nor `nsenter` on the host or in containers.
The optional vnStat and Docker startup sequencing features still invoke
`vnstat` and `systemctl`, respectively. Kernel integration test fixtures use
`ip` and `unshare`, so those tests require iproute2 and util-linux.

## Install as a host service

```bash
sudo install -m 0755 bin/veth-manager /usr/sbin/veth-manager
sudo install -d -m 0755 /etc/veth-manager
# Initial install only: do not overwrite an existing administrator configuration.
sudo test -e /etc/veth-manager/manager.yaml ||
    sudo install -m 0644 deploy/manager.yaml /etc/veth-manager/manager.yaml
sudo test -e /etc/veth-manager/docker.yaml ||
    sudo install -m 0644 deploy/docker.yaml /etc/veth-manager/docker.yaml
# Edit manager.yaml and docker.yaml to enable the desired behaviors before starting.
sudo install -m 0644 deploy/veth-manager.service /etc/systemd/system/veth-manager.service
sudo systemctl daemon-reload
sudo systemctl enable --now veth-manager
sudo journalctl -u veth-manager -f
```

The supplied unit uses **primary-repair mode**. To use alternative names:

```bash
sudo systemctl edit veth-manager
```

Add this override:

```ini
[Service]
ExecStart=
ExecStart=/usr/sbin/veth-manager --manager-config=/etc/veth-manager/manager.yaml --mode=alternative --mapping-file=/run/veth-manager/mappings.json
```

Then run `sudo systemctl daemon-reload && sudo systemctl restart veth-manager`.

The unit deliberately runs on the host as root, with bounded capabilities for
namespace entry and link management. It does not use a privileged container,
mount the Docker socket into a container, or enable a TCP Docker API. Access to
the Docker socket is **root-equivalent**. Interface discovery uses read-only
Docker requests; optional startup sequencing also sends container start
requests. Systemd hardening does not make the Docker socket safe for
untrusted code. Review the binary and labels as privileged configuration.
Do not enable `PrivateNetwork`: that would hide the host interfaces.

## Operation

- Scans running containers at startup and every 30 seconds.
- Names interfaces only when `renaming.enabled` is true. Optional periodic
  orphan DHCP cleanup has its own interval and observation grace period.
- Listens for container/network Docker events, coalesces bursts, and reconciles.
- Reconnects after event-stream failures; full scans cover missed events,
  startup/attachment races, Docker restarts, and removals.
- For bridge networks, matches each Docker endpoint's MAC address against
  interfaces discovered through native rtnetlink **inside the container network
  namespace**, then checks reciprocal peer indices,
  host veth type, and expected bridge membership. No `eth0` assumption or host
  sysfs lookup inside just a network namespace.
- Supports the default bridge, custom Compose bridges, and explicit bridge
  names, plus container-side naming for the macvlan networks described above.
  DHCP plugin bridge mode also supports host-side veth naming.
  Host/shared network namespaces are excluded; other drivers and unsupported
  modes (including overlay, ipvlan, and none) are not managed.
- Revalidates the container PID/endpoint and both ends immediately before a
  change, checks primary and alternative-name collisions, and verifies the
  result afterward.
- Refuses all endpoints that request the same name. Only primary-repair mode
  lowers verified host veths and reclaims proven disconnected DHCP names;
  other modes never lower links. Unrelated names and interfaces are not changed.
- Logs errors as JSON to stderr/journald. Failed endpoints remain visible in
  the mapping snapshot and are retried on later scans. `--once` exits nonzero
  for any discovery, validation, rename, verification, or output error.
- Logs an identical scan failure only once, without duplicate reconciliation
  and retry messages. A changed failure or a failure recurring after a
  successful scan is logged again. Retries and snapshot updates continue
  even when repeat messages are suppressed. Restarting resets this history.
- Logs each unchanged skipped endpoint only once per daemon run, rather than
  on every scan. A changed mapping or an endpoint disappearing and returning
  allows a new skip log. Skipped endpoints remain in every mapping snapshot;
  restarting the daemon resets the log history.

Docker and the kernel do not provide a single transaction spanning inspection,
namespace entry, and renaming. Revalidation narrows races but cannot eliminate
the final check-to-operation window during concurrent endpoint destruction.
An abstract Unix socket prevents simultaneous mutating instances of this
daemon. Dry-run previews can coexist. External network managers do not honor
that lock, so avoid concurrent interface renamers.

This is for local rootful Docker bridge and supported macvlan networks, not Docker Desktop, rootless
Docker, Swarm overlay networking, remote Docker daemons, or Kubernetes.
Monitoring/firewall/tc rules tied to an original primary name may need adjustment
if a primary rename succeeds.

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--socket` | `/var/run/docker.sock` | Local Docker Unix socket |
| `--manager-config` | `/etc/veth-manager/manager.yaml` | YAML opt-ins for renaming and periodic orphan cleanup; both disabled by default |
| `--mode` | `primary-repair` | Naming strategy when `renaming.enabled` is true; `auto`/`alternative` avoid downtime; `primary` is strict |
| `--vnstat` | `auto` | Detect vnStat 2.x if installed; `off` disables, or provide an executable path |
| `--interval` | `30s` | Full scan interval, at least `1s` |
| `--mapping-file` | disabled | Atomic JSON snapshot; parent directory must exist |
| `--once` | false | One scan, JSON to stdout, nonzero on errors |
| `--dry-run` | false | Plan and validate without modifying interfaces |

### Mapping output

The systemd unit writes `/run/veth-manager/mappings.json`. Each snapshot has
`updated_at`, `mode`, `dry_run`, `renaming_enabled`, and a `mappings` array containing:

- Container ID/name, Docker network ID/name, bridge, IPv4, and IPv6.
- Current primary `interface`, `ifindex`, and `requested_name`.
- `namespace`: `host` for bridge veths, `container` for macvlan interfaces.
  Interface names and indices belong to that namespace; macvlan rows have an
  empty `bridge`. The namespace may be absent when discovery fails or a driver
  is unsupported.
- `status`: `applied`, `unchanged`, `monitored`, `would-change`, `skipped`, or `error`.
- `monitored` status also indicates a verified endpoint when renaming is disabled.
- `naming_warning`: auto mode is monitoring a live link instead of applying a name.
- `vnstat_status`: `applied`, `unchanged`, `would-change`, or `error`, when enabled.
- `vnstat_error`: registration, alias, database, or readback failures.
- An `error` explanation when applicable.

In auto/alternative mode, `interface` is still Docker's primary name;
`requested_name` is the alternative name. Errors before peer discovery have
an empty interface and index zero. An unavailable container inspection creates
an error row with its ID. A complete scan removes rows for stopped/deleted
containers. Top-level discovery failures preserve the previous file: check
`updated_at` and journald for freshness. The snapshot is world-readable in the
supplied unit and exposes container names and addresses; restrict the runtime
directory if that is inappropriate for your host.

## Validation on your Ubuntu host

Unit tests cover label validation, multi-network naming, Docker API/events,
MAC/peer/bridge matching, collisions, restart races, dry runs, primary/alternative
idempotency, command failure, post-change verification, and atomic JSON output.
They do not prove live kernel/Docker behavior.

Before enabling the service on important workloads:

1. Set `renaming.enabled: true` and leave cleanup disabled in the selected
   manager configuration. Use a disposable labeled container on a test bridge. Preview with `--once
   --dry-run`; compare the mapping's MAC/peer/bridge against
   `docker inspect` and `ip -j -d link`.
2. Maintain a connection or ping to the container while running `--once --mode=primary`.
   On kernels that reject UP renames, expect a nonzero exit, an `error`
   mapping, the original name, and no deliberate link downtime.
3. Run `--once --mode=alternative`. Check `ip -d link show` for the alternative
   name, test `ip link show dev <alternative-name>`, and verify your actual
   packet capture/monitoring tool accepts it.
4. Run the daemon in alternative mode. Attach/detach a second bridge, restart
   and recreate the container, and verify names/mappings converge. Test both
   network-specific labels and base-derived names.
5. Give two disposable containers the same explicit label. Confirm both are
   reported as conflicts and neither is renamed. Remove the conflict and
   verify the next scan succeeds.
6. Stop/delete the test containers and confirm their mapping rows disappear.
   Restart Docker only on a test host and confirm the event stream reconnects.

No live-host networking or Docker tests are automatically run by `go test`.
On Linux with vnStat 2.x installed, run the isolated real-CLI check with
`VNSTAT_INTEGRATION=1 go test -run TestVNStatCLIIntegration -v`.
It uses only temporary configuration/database files, does not change links,
and does not access the production vnStat database.
The release gate additionally runs `TestPrimaryRepairKernelIntegration` as
root with `PRIMARY_REPAIR_INTEGRATION=1`. It creates its own disposable network
namespace and exercises real bridge/veth naming, reclamation, and rollback
without altering host interfaces.

`TestNativeLinksKernelIntegration`, enabled with `NATIVE_LINKS_INTEGRATION=1`,
also runs in disposable network namespaces. It verifies native interface and
address discovery, state changes, alternative names, and container veth/macvlan
discovery and renaming with an empty `PATH`, including host namespace isolation.

## Publishing releases

[.goreleaser.yaml](.goreleaser.yaml) builds Linux amd64/arm64 binaries, standalone
archives, Debian packages, and SHA-256 checksums. The package declares
`systemd` and `logrotate` dependencies; Docker must already be set
up separately. Package scripts reload systemd but never automatically start,
enable, or restart the service, including during upgrades.

[The GitHub Actions workflow](.github/workflows/release.yml) runs Go tests/vet,
shell syntax checks and ShellCheck, mocked installer tests, a snapshot package
build, and Debian package structure checks before permitting a tagged release.
Both workflow jobs install the pinned GoReleaser tool through GitHub's API and
verify its SHA-256 checksum, avoiding the normal release-download route.
Both jobs use Ubuntu 24.04 to keep the clang-14 toolchain available. The test
job regenerates the eBPF object before running Go and live-kernel tests.

To publish:

1. Commit and push this project to `mayberryjp/veth-manager`, with `main` as the
   default branch. The source must be published for the raw installer URL to work.
2. Push a semantic version tag, for example `v0.1.1`. The workflow creates the
   GitHub Release and uploads `.deb` packages, archives, `checksums.txt`, and
   `install.sh`. It uses the workflow's `GITHUB_TOKEN`; no personal token is needed.
3. Confirm the workflow passed and the release contains
   `veth-manager_0.1.1_amd64.deb` and `veth-manager_0.1.1_arm64.deb` before
   sharing the install command.

Locally, with GoReleaser v2.18.3, Bash, and Debian package tools available:

```bash
go test ./...
go vet ./...
bash tests/installer_test.sh
goreleaser check
goreleaser release --snapshot --clean
bash tests/package_test.sh
```

Snapshot builds require a Git checkout but do not upload or publish anything.
Installer tests stub all downloads, package installation, privilege escalation,
and service control; they never install packages or change host services.
Live package installation/removal and systemd behavior still need verification
on a disposable Ubuntu host.
