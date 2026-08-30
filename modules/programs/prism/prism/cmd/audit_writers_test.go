package cmd

// Tests that `prism audit` names every action that writes audit events.
//
// `prism audit` tells the reader which actions write audit events. Three class
// A documents point a coordinator at that command as the proof of the tier-3
// checkin control, so a footer that omits a writer actively contradicts them.
//
// The enumeration drifts when a writer is added but the footer is not
// updated:
//
//   - Adding a command to the bash-promotion list, while the footer keeps
//     naming the older set.
//   - Adding a second writer entirely (the tier-3 privileged checkin), while
//     the footer names only the bash writer.
//
// This slips through when the string is hand-maintained and nothing
// compares it to the source of truth. These tests do that comparison.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/sidecar"
)

// TestAuditWritersSentence_NamesEveryHighImpactPrefix pins writer 1. Adding a
// prefix to sidecar.HighImpactCommandPrefixes without the enumeration picking
// it up fails here.
func TestAuditWritersSentence_NamesEveryHighImpactPrefix(t *testing.T) {
	prefixes := sidecar.HighImpactCommandPrefixes()
	if len(prefixes) == 0 {
		t.Fatal("sidecar.HighImpactCommandPrefixes() is empty — this test would be vacuous")
	}

	sentence := auditWritersSentence()
	for _, prefix := range prefixes {
		if !strings.Contains(sentence, prefix) {
			t.Errorf("audit writers sentence does not name the high-impact prefix %q.\ngot: %s", prefix, sentence)
		}
	}
}

// TestAuditWritersSentence_NamesThePrivilegedCheckinWriter pins writer 2. The
// tier-3 grant is not a bash command, so it has no prefix in the list above
// and needs its own assertion.
func TestAuditWritersSentence_NamesThePrivilegedCheckinWriter(t *testing.T) {
	sentence := auditWritersSentence()

	if !strings.Contains(sentence, privilegedCheckinWriterClause) {
		t.Errorf("audit writers sentence does not name the tier-3 privileged checkin writer.\ngot: %s", sentence)
	}
	// The clause must remain recognisable to a reader who greps for the verb
	// the three documents send them here with.
	if !strings.Contains(sentence, "prism checkin") {
		t.Errorf("audit writers sentence must name `prism checkin` verbatim — that is the command the coordinator docs send the reader here to verify.\ngot: %s", sentence)
	}
}

// TestAuditCmdLong_CarriesTheWritersSentence keeps the two user-facing
// surfaces in step. The table footer and `prism audit --help` both describe
// the writers, and fixing only one is exactly the defect this file exists to
// prevent.
func TestAuditCmdLong_CarriesTheWritersSentence(t *testing.T) {
	if !strings.Contains(auditCmd.Long, auditWritersSentence()) {
		t.Errorf("`prism audit --help` long text does not carry the audit writers sentence.\nlong text:\n%s", auditCmd.Long)
	}
}
