// The host-API credential (#3379).
//
// A LOCAL ADDITION, not a port. See UPSTREAM.md divergence #23.
//
// The sidecar of a session refuses each host-API request that does not carry
// the secret that it wrote to hostapi.secret, next to its hostapi.sock. The
// extension reads the file at each request, so it gets the new secret of a
// sidecar that restarted. The secret goes in a request header only. Do not
// log it, and do not put it in an argv.

import { readFileSync } from "node:fs"
import { dirname, join } from "node:path"

// Go copy: Header and SecretFileName in prism/internal/hostapiauth. Change
// both sides together. TestHostAPIAuth_ExtensionUsesSameValues fails when
// they differ. HTTP header names do not depend on case.
export const HOST_API_SECRET_HEADER = "x-prism-host-api-secret"
export const HOST_API_SECRET_FILE = "hostapi.secret"

/**
 * The credential header for a request to the host-API socket at socketPath,
 * or an empty object when the secret file cannot be read. The sidecar then
 * refuses the request with 401, and the caller logs the status.
 */
export function hostApiAuthHeaders(socketPath: string): Record<string, string> {
  try {
    const secret = readFileSync(join(dirname(socketPath), HOST_API_SECRET_FILE), "utf8").trim()
    return secret === "" ? {} : { [HOST_API_SECRET_HEADER]: secret }
  } catch {
    return {}
  }
}
