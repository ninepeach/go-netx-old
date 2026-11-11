# sing/common in go-netx: coverage and port plan

Reference: SagerNet/sing `dev` at `27b73774b240` (2026-11-10). go-netx
`main` at `840a674` plus PR #42. Decisions: D-28 (and D-21…D-27 for buf).

## 1. License

sing is **GPL-3.0-or-later** (`LICENSE`, copyright nekohasekai). Copying or
adapting its source would make go-netx a derivative work under the GPL.
go-netx has **no LICENSE file yet**: the owner should choose one. Until
then, and in any case for a permissive license, every capability is
implemented from the operating systems' and Go's documentation, with
go-netx's own tests; sing is read only to list capabilities and
behaviours. sing's own dependencies are `golang.org/x/sys` and testify.

## 2. Module inventory

Status: **covered** (go-netx has it), **partly**, **missing**, **out of
scope** (not general networking infrastructure).

| sing/common | Size (lines, tests) | What it is | go-netx | Status | Plan |
|---|---|---|---|---|---|
| `buf` | 607, 0 | pooled cursor buffer, refcount | `buf` | covered (D-21…D-27); refcount refused (D-25) | none |
| `control` | 1206, 0 | socket options per platform | `socket` | partly → mostly with PR #42 | PR-1 (#42), PR-2 |
| `network` | 1428, 0 | conn/packet interfaces, upstream unwrapping, counters, handshake/early conns | `tcp.Conn`, `udp.Conn`, `stream` | partly: half-close and ReaderFrom/WriterTo (#41) | adopt only what has a go-netx caller; the interface set is sing's protocol-stack plumbing |
| `bufio` | 5754, 6 | copy loops, own splice with counters, vectorised I/O, batch packet I/O (recvmmsg/sendmmsg, GSO), ReadWaiter, NAT, cached/fallback readers | `stream`, `udp` (recvmmsg), `tcp.Conn` (#41, stdlib splice) | partly | PR-4 (UDP batch send + GSO), behind a benchmark |
| `canceler` | 331, 0 | idle timeout that cancels a packet conn's context | `flow` idle expiry, deadlines | partly | none unless a caller needs per-conn idle cancel outside `flow` |
| `metadata` | 700, 0 | `Socksaddr` (IP or domain + port), SOCKS address serializer, sockaddr conversion | `netip` throughout; `internal/sys` sockaddr | partly | serializer is protocol encoding: Ferry's, not go-netx's |
| `task` | 155, 0 | run tasks, first error cancels | none | missing | stdlib + `errgroup` pattern; not worth a package |
| `udpnat2` | 300, 1 | UDP NAT sessions on an LRU with per-entry timeout | `flow.Table` | covered, different policy (full table refuses new flows, D-23, instead of evicting) | none |
| `udpnat` | 259, 0 | older NAT | `flow` | superseded upstream | none |
| `batch` | 108, 0 | concurrent jobs with a limit, collected results | `limit.Concurrency` | partly | none |
| `cache` | 291, 0 | generic LRU with age/stale | none | missing | out of scope until a caller needs it (DNS cache belongs to Ferry) |
| `replay` | 102, 0 | replay filters (cuckoo, simple) for protocol auth | none | out of scope | protocol-level (Shadowsocks-style auth): Ferry |
| `observable` | 134, 0 | event subscription | none | out of scope | — |
| `cleanup` | 94, 1 | hooks `sync` pool cleanup through `go:linkname` | none | refused | `linkname` into the runtime breaks with Go releases |
| `pipe`, `rw`, `binary`, `varbin` | 1970 | pipes, read/write helpers, binary encoding | stdlib | out of scope | — |
| `uot` | 548, 0 | UDP-over-TCP protocol | none | out of scope | protocol: Ferry |
| `domain`, `ntp`, `json`, `logger`, `format`, `exceptions`, `baderror`, `auth`, `abx`, `atomic`, `byteformats`, `random`, `ranges`, `shell`, `debug`, `memory`, `tls` | — | application infrastructure of sing-box | — | out of scope | — |
| `windnsapi`, `wininet`, `winiphlpapi`, `winpowrprof`, `winwlanapi` | 1861 | Windows system APIs | — | out of scope | — |

## 3. control, per API

| sing `control` | go-netx | Status |
|---|---|---|
| `BindToInterface`, `BindToInterfaceFunc`, `UnbindFromInterface` (Linux, darwin, Windows) | `Options.Interface` (#42) | covered; unbind not needed (options apply at creation) |
| `InterfaceFinder`, `NewDefaultInterfaceFinder` (cached name→index) | `net.InterfaceByName` per socket | partly; a cache only if dial rate makes the lookup measurable |
| `RoutingMark` (Linux) | `Options.RoutingMark` (#42) | covered |
| `DisableUDPFragment`, `EnableUDPFragment` | `Options.DontFragment` (#42) | covered (enable = the default) |
| `BindAddressNoPort` (Linux) | `Options.BindAddressNoPort` (#42) | covered |
| `ReuseAddr`, `ReuseAddrOnly` | Go sets `SO_REUSEADDR` on Unix listeners; `Options.ReusePort` | covered |
| `UDPSocketBuffer`, `setSocketBuffer` (FORCE fallback, errors ignored) | `RecvBuffer`/`SendBuffer`, verified (D-16) | covered, stricter |
| `SetKeepAlivePeriod` (Linux) | `Options.KeepAlive` (all platforms, with count) | covered |
| `TProxy`, `TProxyWriteBack`, `GetOriginalDestinationFromOOB` (Linux) | none | missing → PR-2 |
| `GetOriginalDestination` (Linux `SO_ORIGINAL_DST`, darwin pf) | none | missing → PR-2 (Linux); darwin pf ioctl is fragile, decide in PR-2 |
| `DisableUDPNetReset` (Windows `SIO_UDP_CONNRESET`) | Windows reset errors classified transient (`internal/sys`) | covered, different method |
| `IsUDPEOF` (kqueue) | read-error policy in `internal/sys` | to check in PR-2 |
| `ProtectPath` (Android VPN fd protect) | none | out of scope (Android is not a target) |

## 4. Dependencies

None needed so far. Every constant missing from `syscall` is defined from
the platform headers (PR #42). `golang.org/x/sys` would only be worth it
for PR-4 (sendmmsg/GSO) if raw syscall wrappers grow; proposed there with
size and alternatives first.

## 5. Platform risks

- Windows: `IP_UNICAST_IF` takes network byte order and reads back in host
  order (found on CI); it steers outgoing traffic only.
- macOS and FreeBSD refuse IPv4-level options on IPv6 sockets, so IPv4
  traffic through a dual-stack socket is not covered there.
- `SO_MARK` needs CAP_NET_ADMIN; CI runs unprivileged and covers the EPERM
  path, root runs cover the read-back.
- TProxy (PR-2) needs CAP_NET_ADMIN and iptables/nft rules to test end to
  end; CI can only check the options and the OOB parsing.
- GSO (PR-4) depends on kernel version and NIC; it must degrade (D-18).

## 6. PR plan

| PR | Content | Depends on |
|---|---|---|
| PR-1 (#42) | `socket`: Interface, RoutingMark, DontFragment, BindAddressNoPort | — |
| PR-2 | `socket`/`udp`: Linux TProxy (IP_TRANSPARENT, IPV6_TRANSPARENT), original destination for TCP (`SO_ORIGINAL_DST`) and UDP (`IP_RECVORIGDSTADDR` OOB); check kqueue UDP EOF | PR-1 |
| PR-3 | `socket`: interface index cache, only if a dial benchmark shows the lookup cost | PR-1 |
| PR-4 | `udp`: batched send (sendmmsg) and UDP GSO on Linux, with a benchmark against `WriteTo` | — |

Everything else in section 2 is covered, out of scope, or protocol code
that belongs in Ferry.
