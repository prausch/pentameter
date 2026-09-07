# Local control surface (`POST /command`)

> **This is a local-fork feature and must not be included in any upstream PR.**
> Upstream pentameter is a read-only Prometheus exporter by explicit design
> ("No control/write surface" is a stated non-goal). This branch —
> `paul/local-control` — adds a write path for a private homelab deployment.
> Keep it off `paul/expanded-metrics`, which is the branch headed upstream.

## Why this exists

The Pentair IntelliCenter accepts only a small number of simultaneous
connections. Running pentameter (WebSocket `:6680`) alongside Homebridge's
IntelliCenter plugin (Telnet `:6681`) has repeatedly exhausted that limit and
knocked one or both offline.

The goal is to retire the Homebridge plugin and drive the pool from Hubitat
instead. If the Hubitat driver opened its *own* connection, the contention bug
would move rather than be fixed — so control reuses the WebSocket pentameter
already holds open for `RequestParamList` polling. **Adding control adds zero
connections.**

## Enabling it

The endpoint is **opt-in**. Without `PENTAMETER_COMMAND_TOKEN` set, `/command`
returns 404 and the binary behaves exactly as upstream does.

```bash
PENTAMETER_COMMAND_TOKEN=<shared-secret>
```

Requests must carry that value in an `X-Auth-Token` header. Unlike `/metrics`,
this endpoint actuates physical equipment, so it should not be as open as the
read path. The token is a LAN-scoped shared secret, not a real auth system — the
network boundary is doing most of the work.

## Requests

`POST /command`, JSON body, **exactly one action per request**.

| Action | Body | Writes |
|---|---|---|
| Switch a circuit | `{"circuit": "C0006", "state": "off"}` | circuit `STATUS` |
| Enable/disable heat | `{"body": "pool", "heat": "on"}` | body `HEATER` (see below — **not** `HTSRC`) |
| Set heat setpoint | `{"body": "pool", "setpoint": 82}` | body `LOTMP` |

`circuit` accepts either the objnam (`C0006`) or the friendly name pentameter
already tracks for metric labels (`Pool Light`), case-insensitively. `body`
accepts the friendly name (`Pool`), the subtype (`POOL`), or the objnam
(`B1101`).

`state` and `heat` accept `on`/`off`, `1`/`0`, `true`/`false`.

### Heat is a property of the body, not the heater

The heater object has no on/off of its own. Heat is **enabled** by assigning a
heater to a body and **disabled** by clearing that assignment (`00000`).

**Write `HEATER`, never `HTSRC`.** The body carries both, and both *report* the
assigned heater objnam, but only `HEATER` is settable. A `SetParamList` writing
`HTSRC` is rejected with response `404` and an empty description, while `LOTMP`
on the same object over the same command succeeds — the controller is refusing
that specific parameter, not the command or the object. `HTSRC` is the reported
heat source and follows `HEATER` on its own; verified live, writing
`HEATER=00000` moved `HTSRC` to `00000` and `HTMODE` from `2` to `0`.

With a single installed heater you can omit the target. With more than one, the
request is rejected rather than guessed — pass `{"heater": "Solar Heater"}` (or
its objnam) to disambiguate. Picking whichever heater a map happened to iterate
first would actuate the wrong equipment.

### Setpoint bounds

Setpoints outside **40–104°F** are rejected. IntelliCenter itself accepts a
wider range than is ever sensible for a pool, and a bad value written over the
wire is a physical-equipment problem, not a display bug.

## Responses

Always a JSON object, on success and failure alike:

```json
{"status": "ok", "applied": "circuit C0006 (Pool) -> OFF"}
{"status": "error", "error": "setpoint 200.0 out of range (40-104°F)"}
```

| Status | Meaning | Client should |
|---|---|---|
| 200 | Applied | — |
| 400 | Malformed or invalid request | Fix the request; do not retry |
| 401 | Bad or missing `X-Auth-Token` | Do not retry |
| 404 | Control not enabled on this deployment | Do not retry |
| 405 | Not a POST | Do not retry |
| 503 | IntelliCenter link is down | **Retry** |

The 400/503 split matters for a polling driver: 503 is transient and worth
retrying, 400 never is.

## Examples

```bash
TOKEN=<shared-secret>
HOST=http://pentameter:8080

# Turn the pool light off
curl -sS -X POST "$HOST/command" -H "X-Auth-Token: $TOKEN" \
  -d '{"circuit": "Pool Light", "state": "off"}'

# Enable solar heat on the pool body
curl -sS -X POST "$HOST/command" -H "X-Auth-Token: $TOKEN" \
  -d '{"body": "pool", "heat": "on"}'

# Set the heat setpoint to 82°F
curl -sS -X POST "$HOST/command" -H "X-Auth-Token: $TOKEN" \
  -d '{"body": "pool", "setpoint": 82}'
```

## `GET /debug/params` — reading the controller raw

Same token as `/command`. Read-only, but it discloses the full equipment layout,
so it is gated identically.

```bash
curl -sS "$HOST/debug/params?objtyp=BODY" -H "X-Auth-Token: $TOKEN"
curl -sS "$HOST/debug/params?objtyp=HEATER&keys=SNAME,STATUS,BODY,SUBTYP" -H "X-Auth-Token: $TOKEN"
```

`objtyp` is required (`BODY`, `HEATER`, `CIRCUIT`, `PUMP`, …). `keys` is optional
and defaults to a broad superset; the controller ignores keys an object does not
have, so over-asking costs nothing.

**Reading the output — the echo convention.** A parameter an object does not have
is returned with its own name as the value. On a HEATER object, `"HTSRC": "HTSRC"`
and `"TEMP": "TEMP"` mean *absent*, while `"BODY": "B1101"` and `"STATUS": "ON"`
are real values. Without this rule the dump looks like it contains far more
populated fields than it does.

This endpoint exists because the write path had to be reverse-engineered, and a
rejected `SetParamList` reports only a numeric code (`200` success, `400` bad
request, `404` unknown command — often with no description at all). Diagnosing by
trying candidate parameter names against live pool equipment is not acceptable;
read the object first.

## Confirming a command took effect

Polling runs on `PENTAMETER_INTERVAL` (60s in the homelab deployment), so a
command's effect can take up to a full interval to appear in `/metrics`. A
200 response means the IntelliCenter acknowledged the write, not that the
exporter has re-read it yet.

A client that flips a switch and immediately re-reads `/metrics` will see the
old value. Drivers should either optimistically reflect the commanded state or
wait out the interval — do not treat the stale read as a failed command.

## Concurrency note for future changes

`connMu` serializes the entire write-then-read cycle on the shared WebSocket.
The polling loop runs in its own goroutine while the HTTP server serves on
another, and every IntelliCenter call is a `WriteJSON` followed by a
messageID-correlated `ReadJSON` — without the lock, two goroutines interleave
and read each other's responses.

**Any new code path that touches `pm.conn` must take `connMu` for the whole
cycle, not just the write.** `PoolMonitor.mu` is a different lock guarding
listen mode, which does not start the HTTP server and is therefore unaffected.
