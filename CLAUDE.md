# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`github.com/mhashemm/upnp` — a zero-dependency Go library (stdlib only, Go 1.23.3) that opens/closes router port mappings via UPnP IGD. Three public operations: `AddPortMapping`, `DeletePortMapping`, `GetExternalIPAddress`.

## Commands

```bash
go build ./...
go vet ./...
go test ./...         # full suite, offline
go run ./playground   # manual smoke test — needs a real UPnP router on the LAN

# verify against the real router with access to unexported state:
# write a throwaway zz_live_test.go in package upnp, `go test -run Live -v`, then rm it.
# playground/ is package main and cannot reach c.service.
```

`upnp_test.go` covers the package: URL resolution, the M-SEARCH bytes, SSDP parsing, dedupe and gateway-first ordering, UDP retransmission, the proxy-free transport, XML tolerance (latin-1 included), SCPD action discovery and the WAN fallback, service ranking and live-WAN selection, the SOAP envelope's namespace prefixes, fault decoding (in a 200 too), the 725 retry and its reported lease, and the three client operations. It runs offline — `httptest` fake routers plus a loopback UDP responder — except `TestGetLocalIPAddr`, which skips without a route. `New` and `discover` are the only uncovered functions: both need real multicast, so they stay on the manual `playground/` path (gitignored, not part of the module's public surface). `go test -short` skips the one test that waits out the 6.5s SSDP deadline.

## Conventions

- **No comments in `upnp.go`/`utils.go`/`structs.go`.** The source is deliberately comment-free; explain things here instead. `upnp_test.go` is the exception — its comments record which router bug each test pins.

## Architecture

Everything is one flat package (`upnp.go` logic, `utils.go` transport helpers, `structs.go` wire types). The whole library is the four-stage discovery pipeline in `New()`:

1. **`GetLocalIPAddr`** — dials a UDP socket to a bogus address (`6.9.6.9:6969`) purely to make the kernel pick the outbound interface IP. Nothing is sent.
2. **`discover`** — multicasts `mSearch`, a literal SSDP `M-SEARCH` datagram in utils.go, to `239.255.255.250:1900`, then collects every reply until one absolute deadline (6.5s) expires. It is a literal because `net/http` cannot serialize a non-HTTP method to `*`, and because building it from a map shuffled the header order from run to run. `parseSSDP` reads each reply with `http.ReadResponse`, dedupes by `LOCATION`, and moves gateways to the front (see `ST: ssdp:all` below).
3. **`upnpService`** — for each replying device (deduped by `LOCATION`), GETs its device-description XML, then walks the nested `device`/`deviceList` tree (`getConnectionService`, recursive) and probes each service's `SCPDURL`. A service qualifies if its action list contains one of the three supported actions (`serviceActions`), or if it is a `WANIPConnection`/`WANPPPConnection` whose SCPD could not be used. `getConnectionService` returns *every* qualifying service; `connectedService` then picks the one whose `GetExternalIPAddress` answers with something other than `""`/`0.0.0.0`, because routers list both `WANIPConnection` and `WANPPPConnection` with only one live. First qualifying service is the fallback when none look connected. Failures accumulate into an `errors.Join`. A device that yields no candidate and no error adds an error of its own: `errors.Join` of only nils is `nil`, and `New` would otherwise return a client with no service.
4. Everything after discovery is SOAP over HTTP: `newEnvelopeReq` marshals the request struct into a literal envelope and sets the `SOAPAction` header, `doRequest` decodes the reply with `decodeXML`.

Consequences worth knowing before changing things:

- **Best-effort by design.** Discovery tries every device and every service, joining errors rather than failing on the first. Preserve that when touching `upnpService`/`getConnectionService`.
- **`body` decodes responses only** — one nullable pointer per response type, plus `Fault`. Adding a new UPnP action means: a const in `structs.go`, request/response structs, one field on `body`, and a `Client` method following the existing three (they are deliberately near-identical; do not abstract them behind a generic helper for its own sake).
- **Request structs carry `XMLName xml.Name` with the `u:` prefix** (`xml:"u:AddPortMapping"`) and an `xmlns:u` attr set from `service.ServiceType` at call time; response fields have no prefix. This asymmetry is what IGD routers expect. Drop the prefix and routers answer `401 Invalid Action`.

- **The envelope is a string constant, not a struct** (`envelopeOpen`/`envelopeClose`). `encoding/xml` cannot emit a prefixed element, so marshalling an `envelope` struct invents an `xmlns:envelope` prefix and puts `Envelope`/`Body` in the default namespace, which strict routers reject. Decoding still goes through the struct, which handles namespaces fine.

- **Errors from the router are `*Error`**, decoded from the SOAP fault whatever the HTTP status: a fault in a 200 is still a failure. `AddPortMapping` retries once with `NewLeaseDuration: 0` on `ErrOnlyPermanentLease` (725), since many routers refuse timed leases outright. `AddPortMappingResponse.LeaseDuration` (`xml:"-"`, set from the request actually sent) tells the caller the lease it got, so it learns when its mapping became permanent and will outlive a crash.

- **A zero `NewEnabled` is sent as 1**, the same kind of default as `NewInternalClient`. The zero value asks for a disabled mapping: miniupnpd ignores the argument, but a stack that honours it answers 200 and forwards nothing.

- **URLs from device descriptions go through `resolveURL`**, never string concatenation: `controlURL`/`SCPDURL` are legally absolute, absolute-path, or relative to the description's own path.
- **All router XML goes through `decodeXML`**, which covers descriptions, SCPDs and SOAP replies: `Strict=false` and HTML entities for sloppy firmware, and `latin1` as `CharsetReader`. encoding/xml refuses any non-UTF-8 encoding declaration outright, and passing the bytes through fails on the first non-ASCII byte as invalid UTF-8, so `latin1` maps each byte to its rune. Every field the library reads is ASCII, so a document with the wrong label still decodes.
- **A service qualifies on *any* of the three actions**, not all — some routers advertise partial action lists. `serviceActions` records which ones it saw in `service.Actions`, and `getConnectionService` sorts by that count so a complete service outranks a `GetExternalIPAddress`-only one. It is a ranking signal and never a veto: a router whose SCPD omits an action it actually implements must still be able to run it, so the router's `401` stays the authority on what works. That holds when the SCPD fails outright too: a `WANIPConnection`/`WANPPPConnection` whose SCPD 404s, times out, or lists none of the three is kept with zero actions and ranks last. miniupnpc never reads the SCPD (it picks by `serviceType`), so a router can ship a broken one and work with everything else.
- `SOAPAction` is set by direct map assignment (`req.Header["SOAPAction"]`), not `Header.Set`, to keep the casing that canonicalization would mangle — routers need `SOAPAction`, not `Soapaction`. staticcheck flags reads of it (SA1008); assert on `Header.Write` output instead of suppressing. The M-SEARCH avoids the issue by being a literal.

- **`ST: ssdp:all`, deliberately.** Narrowing to `InternetGatewayDevice:1` looks tidier but IGD2 routers do not answer it. The costs of `ssdp:all` are paid in `parseSSDP`, not by narrowing the search. One reply per service is collapsed by the `LOCATION` dedupe. Every UPnP device on the LAN answering in random order is handled by stable-sorting locations whose `ST` named `InternetGatewayDevice`/`WAN*Connection` in *any* reply to the front; otherwise the router waits behind each TV's description and SCPDs, or 10s per device that stalls. The flag is recorded before the dedupe `continue` because a router's first reply is often `upnp:rootdevice`. Nothing is filtered out, so a router that only answers `upnp:rootdevice` is still tried.

- **The M-SEARCH is retransmitted 3x** because multicast loss is indistinguishable from "router has no UPnP", and **every HTTP call goes through `httpClient`** with its timeout: a device that accepts the connection and never answers would otherwise hang `New()` forever. `httpClient` has its own `http.Transport`, not `http.DefaultTransport`. It uses no proxy: `ProxyFromEnvironment` does not exempt private addresses, so router traffic followed `HTTP_PROXY` to a proxy that cannot reach the LAN. It uses no keep-alive, matching miniupnpc's one request per connection, so discovery does not hold an idle connection to every device it probed.

- **The SSDP read deadline is absolute, not per-read** (`deadline` in `udpRequest`, `ssdpRetryGap*ssdpRetries + ssdpMX + ssdpMargin`). `New()` looks slow because it sits idle for most of that 6.5s, and resetting the deadline on each packet to "finish early" is wrong twice: it is unbounded — a chatty device holds the loop open forever — and `MX: 5` explicitly invites devices to wait up to 5s, so an idle timeout discards the slow repliers the protocol told to be slow. `MX` in `mSearch` is derived from `ssdpMX` so the advertised window and the deadline cannot drift apart.

## Reference

Protocol background in `readme`: <https://github.com/Self-Hosting-Group/wiki/wiki/Port-Mapping-Protocols-Overview> and RFC 6887 (PCP).
