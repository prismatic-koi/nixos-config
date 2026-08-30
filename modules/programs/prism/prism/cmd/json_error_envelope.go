package cmd

// json_error_envelope.go — shared helper for the prism `--json` error
// contract. Subcommands that expose a `--json` (or
// `--format json`) surface MUST emit errors as a single-line JSON object
// `{"error":"<message>"}` on stderr, leaving stdout empty and exiting
// non-zero. This file centralises the helper so the contract is
// implemented identically across `prism stats compare`, `prism escalate`,
// and any future subcommand that opts in.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// emitJSONErrorEnvelope writes a single-line JSON object of the form
// `{"error":"<message>"}` to stderr and returns a quietExitErr wrapping the
// original error so main's fallback error printer does not duplicate it on
// stderr. The returned error still satisfies the exitCoder interface (exit
// code 1), preserving the non-zero exit-code contract.
//
// Callers typically pair this with cobra's SilenceUsage/SilenceErrors flags
// so cobra does not also dump the usage block on the error path — see the
// `compareCmd` / `abtestCmd` init() in stats_compare.go for the canonical
// pattern.
func emitJSONErrorEnvelope(err error) error {
	return emitJSONErrorEnvelopeTo(os.Stderr, err)
}

// emitJSONErrorEnvelopeTo is the writer-injectable variant used by tests.
// It is otherwise identical to emitJSONErrorEnvelope.
func emitJSONErrorEnvelopeTo(stderr io.Writer, err error) error {
	if err == nil {
		return nil
	}
	payload := map[string]string{"error": err.Error()}
	if b, mErr := json.Marshal(payload); mErr == nil {
		fmt.Fprintln(stderr, string(b))
	} else {
		// json.Marshal of a string map should never fail in practice;
		// the fallback below preserves the contract (single JSON line on
		// stderr) for the pathological case so callers still get a
		// parseable envelope rather than a panic / dropped error.
		fmt.Fprintf(stderr, "{\"error\":%q}\n", err.Error())
	}
	return &quietExitErr{inner: err}
}
