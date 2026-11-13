---
name: unifi
description: Load this skill before you read data from the UniFi controller (the UCG Ultra) — clients, devices, Wi-Fi, networks, firewall policies, traffic, or the system log. It gives the login steps, the safety rules, and the endpoints that work on this controller (Network 11).
---

# UniFi: read the state of the network

Use the login-based API of UniFi OS. The session logs in as a local user
with "Network View Only" and "Control Plane View Only". The session has
these variables:

- `UNIFI_HOST`: the host name of the controller. A reverse proxy with a
  valid certificate serves it, so curl verifies TLS.
- `UNIFI_USERNAME`: the local user.
- `UNIFI_PASSWORD_FILE`: the path of the file that holds the password.

If prism did not set `UNIFI_PASSWORD_FILE`, this session has no UniFi access. Stop,
and tell the user. A review agent does not get these variables.

## Rules

1. Send only read requests. The login, the logout, and the read endpoints
   that take a JSON body (`stat/sitedpi`, `stat/report/*`, the v2
   `system-log`, the v2 `traffic-flows`) use POST.
2. Do not send POST, PUT, or DELETE to `rest/*`. Do not send a request to
   `cmd/*`. The controller refuses a write from this account with 403. Do
   not rely on that.
3. Do not print the password, the response headers, the cookie jar, or the
   CSRF file `$U/csrf`. Do not put the password on a command line. Read the
   file inside the command, as in step 1.
4. Do not use `curl -v`, `--verbose`, `--trace`, `--trace-ascii`, `-i`,
   `--include`, or `-D -`. These options print headers, and the headers hold
   the session token.
5. If curl cannot reach `$UNIFI_HOST`, or TLS verification fails, tell the
   user and stop. Do not use an IP address in place of `$UNIFI_HOST`. Do not
   add `-k` or `--insecure`.
6. The redactor does not mask `UNIFI_HOST`. Do not copy output that shows
   the host name into a commit, a PR, an issue, or a file in a public repo.
   Write `$UNIFI_HOST` there.
7. Use only the endpoints in this skill. Each one has a test result on this
   controller (#3473, #3476). Do not add an endpoint from community
   documentation. Some community endpoints do not exist on Network 11.
8. Use the API for current state. For history, query Loki with
   `{job="unifi-syslog"}`. home-ops #4145 delivers this stream.
9. Log out when the work is done.

## Steps

Each bash command starts in a new shell. Keep the cookie jar and the CSRF
file in a directory that is unique to this session. Make it with `mktemp -d`,
as in step 1. Do not use a fixed name such as `${TMPDIR:-/tmp}/unifi`: two
host-mode sessions share `/tmp`, and they then use the same files.

1. Make the directory and log in. The command prints the path of the
   directory, and then the HTTP status. The expected status is `200`.

   ```sh
   U="$(mktemp -d "${TMPDIR:-/tmp}/unifi.XXXXXXXX")" && echo "$U"
   jq -n --rawfile p "$UNIFI_PASSWORD_FILE" --arg u "$UNIFI_USERNAME" \
     '{username: $u, password: ($p | rtrimstr("\n")), remember: false}' |
     curl -s -o /dev/null -w '%{http_code}\n' -c "$U/jar" -D "$U/headers" \
       -H 'Content-Type: application/json' --data-binary @- "https://$UNIFI_HOST/api/auth/login"
   grep -i '^x-csrf-token:' "$U/headers" | cut -d' ' -f2 | tr -d '\r' > "$U/csrf"
   rm "$U/headers"
   ```

   Start each later command with these variables. Write the path that step 1
   printed in place of `<dir>`:

   ```sh
   U=<dir> S=/proxy/network/api/s/default V=/proxy/network/v2/api/site/default
   ```

2. Send a request with the cookie and the CSRF header. A response of a `$S`
   endpoint holds its rows in `.data`.

   ```sh
   curl -s -b "$U/jar" -H "X-CSRF-Token: $(cat "$U/csrf")" \
     "https://$UNIFI_HOST$S/stat/sta" | jq '.data[] | {mac, hostname, ip, sw_port, assoc_time}'
   ```

   For an endpoint that takes a JSON body, add the body. "Request bodies"
   below gives the body of each endpoint.

   ```sh
   curl -s -b "$U/jar" -H "X-CSRF-Token: $(cat "$U/csrf")" -H 'Content-Type: application/json' \
     --data '{"type":"by_app"}' "https://$UNIFI_HOST$S/stat/sitedpi" | jq '.data'
   ```

   If a request returns 401, log in again.

3. Log out, and delete the directory.

   ```sh
   curl -s -o /dev/null -w '%{http_code}\n' -b "$U/jar" -H "X-CSRF-Token: $(cat "$U/csrf")" \
     -X POST "https://$UNIFI_HOST/api/auth/logout"
   rm -rf "$U"
   ```

## Request bodies

The tests of #3473 and #3476 used the `stat/sitedpi` body. The coordinator
tested the `system-log/all` and `traffic-flows` bodies on 2026-11-14. Each
request returned HTTP 200.

| Request | Body |
|---|---|
| `POST $S/stat/sitedpi` | `{"type":"by_app"}` |
| `POST $V/system-log/all` | `{"pageNumber":0,"pageSize":20,"timestampFrom":0,"timestampTo":9999999999999}` |
| `POST $V/traffic-flows` | `{"pageNumber":0,"pageSize":20}` |

`timestampFrom` and `timestampTo` are milliseconds since the epoch. The body
in the table covers all time. Set the two values to the time window that you
need. This example reads the system log of the last hour. Start it with the
variables of step 1.

```sh
now=$(($(date +%s) * 1000))
curl -s -b "$U/jar" -H "X-CSRF-Token: $(cat "$U/csrf")" -H 'Content-Type: application/json' \
  --data "{\"pageNumber\":0,\"pageSize\":20,\"timestampFrom\":$((now - 3600000)),\"timestampTo\":$now}" \
  "https://$UNIFI_HOST$V/system-log/all" | jq
```

## Endpoints that work

| Request | Result | Use it to |
|---|---|---|
| `GET /api/users/self` | 200 | Make sure that the login works. |
| `GET $S/self` | 200 | Make sure that the account can read the site. |
| `GET $S/stat/health` | 200 | Get the health of the site. Start here. |
| `GET $S/stat/device` | 200. Per-radio stats, uplink, CPU, memory, uptime | Examine an AP, a switch, or the gateway: a radio, an uplink, the load, or a restart. |
| `GET $S/stat/sta` | 200. For each current client: `mac`, `ip`, `hostname`, `name`, `oui`, `network`, `sw_mac`, `sw_port`, `last_uplink_name`, `assoc_time`, `uptime`, `last_seen`, signal, satisfaction, tx/rx rate, channel, AP | Find a client, its AP or switch port, and its Wi-Fi quality. |
| `GET $S/stat/user/<mac>` | 200. `first_seen`, `last_seen`, `fixed_ip`, `use_fixedip` | Examine one known client, also when it is offline. |
| `GET $S/stat/rogueap` | 200 | Find nearby APs that can cause interference. |
| `POST $S/stat/sitedpi` with `{"type":"by_app"}` | 200 | Get the traffic for each application. |
| `GET $S/rest/networkconf` | 200 | Read the networks and the VLANs. |
| `GET $S/rest/wlanconf` | 200 | Read the Wi-Fi networks. |
| `GET $V/clients/active` | 200 | List the current clients in the v2 form. |
| `GET $V/firewall-policies` | 200 | Find the policy that allows or blocks traffic. |
| `POST $V/system-log/all` with the body in "Request bodies" | 200 | Read the system log of a time window, in place of `stat/event` and `stat/alarm`. |
| `POST $V/traffic-flows` with the body in "Request bodies" | 200 | Read the recent traffic flows. |
| `GET $S/rest/portconf` | 200, no rows | Read the switch port profiles. |
| `GET $S/stat/routing` | 200, no rows | Read the routes. |
| `POST $S/stat/report/hourly.site`, `5minutes.ap`, `archive.speedtest` | 200, no rows. The tests sent no time range. No test verified that the request needs one. | Read statistics over time and speed test results. |

## Requests that do not work

| Request | Result |
|---|---|
| Any `/proxy/network/integration/v1/...`, for example `GET /proxy/network/integration/v1/sites` | 401. The Integration API accepts only an API key in the `X-API-KEY` header, not a session cookie. This account has no key: only an admin can create a key, and an admin key can write. |
| `GET $S/stat/event`, `GET $S/stat/alarm`, `POST $S/stat/event` | 404. Network 11 does not have these endpoints. Use `POST $V/system-log/all`, or Loki for history. |
| `POST $V/system-log/client-connection` | 404. Use `POST $V/system-log/all`. |
| `POST $S/rest/networkconf` with `{}`, `POST $S/cmd/stamgr` | 403. The controller refused the write. Do not send these requests. |

## Fields

- `assoc_time` in `stat/sta` shows a reconnect. For a wired client,
  `assoc_time` agreed with an outage in Home Assistant.
- `latest_assoc_time` and `_uptime_by_usw` change when the switch refreshes
  its port records. They are not evidence of a reconnect.
