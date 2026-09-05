package archive

// abtest.go — programmatic A/B test pair loading for downstream tooling.
//
// LoadAbtestPair returns structured data for both sibling sessions of an A/B
// test pair identified by its shared abtest_pair_id. This is the primary API
// for downstream tooling (retro agent, dashboard) that needs to compare the
// two sessions without needing to know about the underlying DB schema.
//
// The caller is responsible for opening and closing the DB. This function is
// intentionally stateless — it does not perform any I/O beyond the DB query.

import (
	"fmt"

	"github.com/prismatic-koi/prism/internal/db"
)

// AbtestPair holds the structured data for both sibling sessions of an
// abtest pair. Either SessionA or SessionB may reflect a missing sibling
// (MissingA / MissingB flags set) when one session was cleaned up before
// the pair data could be read.
type AbtestPair struct {
	PairID string

	// Sessions are ordered by started_at ASC: A is the earlier-spawned session.
	SessionA *db.Session
	SessionB *db.Session

	// SpawnInputs for each sibling (nil when spawn_inputs row is missing).
	InputsA *db.SpawnInputs
	InputsB *db.SpawnInputs

	// SpawnOutcome for each sibling, from db.CompareRunOutcome: the persisted
	// row when it carries the computed aggregates, or a live aggregation for a
	// terminal session whose row does not (no row yet, or a stub written by a
	// partial writer). nil only for a session that is still live.
	//
	// Not SpawnOutcomeByInstanceID. This is a pre-merge A/B comparison surface,
	// so it reads during exactly the window in which the persisted row is
	// absent or a stub — reading the row directly reported zero tokens, zero
	// turns, and no duration for both legs (issue #2932).
	OutcomeA *db.SpawnOutcome
	OutcomeB *db.SpawnOutcome

	// MissingA / MissingB are set when the corresponding sibling session row
	// could not be found (cleaned up or never created). When set, the
	// corresponding Session / Inputs / Outcome fields are nil.
	MissingA bool
	MissingB bool
}

// LoadAbtestPair returns an AbtestPair for the given pairID from the DB.
//
// The pair is looked up by querying spawn_inputs for sessions with
// abtest_pair_id = pairID, joined to sessions, spawn_inputs, and spawn_outcome.
// Sessions are ordered by started_at ASC: index 0 is SessionA, index 1 is
// SessionB.
//
// When only one sibling is found (the other was cleaned up), the surviving
// sibling is returned in SessionA and MissingB is set to true.
//
// When pairID is not found at all, a non-nil error is returned.
func LoadAbtestPair(d *db.DB, pairID string) (*AbtestPair, error) {
	sessions, err := d.SessionsByAbtestPairID(pairID)
	if err != nil {
		return nil, fmt.Errorf("archive: LoadAbtestPair %q: %w", pairID, err)
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("archive: LoadAbtestPair %q: pair not found", pairID)
	}

	pair := &AbtestPair{PairID: pairID}

	// Populate SessionA.
	sA := sessions[0]
	pair.SessionA = &sA
	pair.InputsA, _ = d.SpawnInputsByInstanceID(sA.InstanceID)
	pair.OutcomeA = d.CompareRunOutcome(&sA)

	// Populate SessionB if present.
	if len(sessions) >= 2 {
		sB := sessions[1]
		pair.SessionB = &sB
		pair.InputsB, _ = d.SpawnInputsByInstanceID(sB.InstanceID)
		pair.OutcomeB = d.CompareRunOutcome(&sB)
	} else {
		pair.MissingB = true
	}

	return pair, nil
}
