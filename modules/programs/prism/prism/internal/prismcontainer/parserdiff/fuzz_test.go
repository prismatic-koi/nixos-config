package parserdiff

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
)

// bypassSeeds are the inputs that bypassed an earlier revision of the
// check in the review rounds of PR #3075, and the forms around them. The
// seed run checks each one on every "go test", also without -fuzz.
func bypassSeeds() []string {
	const arg = "ARG X=tarball:/home/u/x.tar\nFROM alpine\n"
	const heredoc = "RUN <<'T\\'\necho hi \\\nT\\\n"
	seeds := []string{
		// Parser split: 0x85 and 0xA0 bytes, also inside UTF-8 (U+0160 is
		// C5 A0, U+0105 is C4 85), and the dotted I of ONBUILD.
		"FROM alpine\nCOPY --exclude=x\u0160--from=tarball:/p /a /b\n",
		"FROM alpine\nCOPY --chown=0\u0105--from=tarball:/p /a /b\n",
		"FROM alpine\nRUN --network=none\u0160--mount=type=bind,from=tarball:/p,target=/m true\n",
		"FROM --platform=linux/amd64\u0160tarball:/p\n",
		"FROM alpine\nCOPY \x85--from=tarball:/p /a /b\n",
		"FROM alpine\nRUN \xa0--mount=type=bind,from=tarball:/p,target=/m true\n",
		"FROM alpine\nONBU\u0130LD COPY --from=tarball:/p /a /b\n",
		// Zone desync: the escaped or expanded flag name.
		"FROM alpine\nCOPY --exclude='\\'\\' z' --from\\\\=tarball:/x /etc/passwd /out\n",
		"FROM alpine\nRUN --network='\\'\\' z' --mou\\\\nt=type=bind,from=tarball:/x,target=/t true\n",
		"FROM alpine\nCOPY --exclude='\\'\\' z' --from$E=tarball:/x /a /b\n",
		"FROM alpine\nCOPY --from\\=tarball:/x /a /b\n",
		"FROM alpine\nCOPY --from\\\\=tarball:/x /a /b\n",
		"FROM alpine\nRUN \xa0--mo${X}unt=type=bind,from=tarball:/home/u/x.tar,target=/m cat /m/f\n",
		"FROM alpine\nRUN \xa0--mou'\"nt\"'=type=bind,from=tarball:/home/u/x.tar,target=/m cat /m/f\n",
		"FROM alpine\nRUN \x85--mou\\\\nt=type=bind,from=tarball:/home/u/x.tar,target=/m cat /m/f\n",
		"FROM alpine\nCOPY \xa0--fr${X}om=tarball:/home/u/x.tar /a /b\n",
		"FROM alpine\nCOPY --fr\"om\"=tarball:/x /a /b\n",
		"FROM alpine\nCOPY --fr\\om=tarball:/x /a /b\n",
		// ARG forms.
		"ARG \"B=tarball:/x\"\nFROM $B\n",
		"ARG B\\=tarball:/x\nFROM $B\n",
		"ARG N=B\nARG ${N}=tarball:/x\nFROM $B\n",
		"ARG V=B=tarball:/x\nARG $V\nFROM $B\n",
		"FROM \"tarball:/x\"\n",
		"FROM tar\\ball:/x\n",
		"FROM alpine\nCOPY --from=\"tarball:/x\" /a /b\n",
		"FROM alpine\nCOPY --from=tar\\ball:/x /a /b\n",
		"FROM alpine\nCOPY --from=$SRC /a /b\n",
		"FROM alpine\nRUN --mount=type=bind,\"from=tarball:/x\",target=/m true\n",
		"FROM alpine\nRUN --mount=type=bind,${K}=tarball:/x,target=/m true\n",
		"FROM alpine\nRUN --mount=type=bind,from=$SRC,target=/m true\n",
		// Physical lines: the heredoc terminator that ends in a backslash,
		// the split forms after it, and leading whitespace.
		arg + heredoc + "FROM $X\n",
		arg + heredoc + "COPY --from=$X / /x\n",
		arg + heredoc + "RUN --mount=type=bind,from=$X,target=/m cat /m/f\n",
		arg + "RUN <<-'T\\'\necho hi\nT\\\nFROM \"$X\" AS y\n",
		arg + heredoc + "FR\\\nOM $X\n",
		arg + heredoc + "COPY \\\n--fr\\\nom=$X / /\n",
		arg + heredoc + "COPY --from=tar\\\nball:/x / /\n",
		arg + "\vFROM $X\n",
		"\v# escape=`\n" + arg + "RUN echo a b \\\nFROM $X\n",
		"\u00a0# escape=`\nFROM $X\nFROM alpine\n",
		"\u3000# escape=`\nFROM alpine\n",
		"# escape=`\nFROM tarball:/x\n",
		// Odd lines and encodings.
		"# a comment \\\nFROM tarball:/x\n",
		"\ufeffFROM tarball:/x\n",
		"\fFROM\vtarball:/x\n",
		"FROM alpine\r\nCOPY --from=tarball:/x /a /b\r\n",
		"from tarball:/x\n",
		"FROM alpine\nCOPY --FROM=tarball:/x /a /b\n",
		"FROM \\\n# a comment\n  tarball:/x\n",
		"FROM alpine\nRUN true" + strings.Repeat(" \\\n  && true", 200) + "\nFROM tarball:/x\n",
		// Inputs that must pass the oracle without a transport.
		"FROM docker://alpine\n",
		"FROM docker:24-dind\n",
		"FROM alpine\nCOPY --from=0 /x /x\n",
		"FROM python:3.12\nRUN <<EOF python3\nfrom typing import List, Dict\nEOF\n",
	}
	transports := []string{"atomic", "containers-storage", "dir", "docker-archive", "docker-daemon", "oci", "oci-archive", "ostree", "sif", "tarball"}
	places := []string{
		"FROM %s:/home/u/x.tar\n",
		"FROM --platform=$BUILDPLATFORM %s:/home/u/x.tar AS base\n",
		"FROM alpine\nCOPY --from=%s:/home/u/x.tar /a /b\n",
		"FROM alpine\nCOPY --from %s:/home/u/x.tar /a /b\n",
		"FROM alpine\nADD --from=%s:/home/u/x.tar /a /b\n",
		"FROM alpine\nRUN --mount=type=bind,from=%s:/home/u/x.tar,target=/m cat /m/f\n",
		"FROM alpine\nONBUILD COPY --from=%s:/home/u/x.tar /a /b\n",
		"FROM alpine\nRUN \\\n  --mount=type=bind,from=%s:/home/u/x.tar,target=/m true\n",
	}
	for _, tr := range transports {
		for _, p := range places {
			seeds = append(seeds, fmt.Sprintf(p, tr))
		}
	}
	return seeds
}

// differ returns a message when imagebuilder finds an image source with a
// transport other than docker:// and the check admits the file.
func differ(data []byte) string {
	sources, ok := ImageSources(data)
	if !ok {
		return ""
	}
	for _, s := range sources {
		if tr := Transport(s); tr != "" && prismcontainer.CheckContainerfile(data) == nil {
			return fmt.Sprintf("imagebuilder reads the source %q (transport %s) and the check admits the file %q", s, tr, data)
		}
	}
	return ""
}

// FuzzCheckAgainstImagebuilder fails when the check admits a Containerfile
// in which imagebuilder finds an image source with a transport other than
// docker://. The check may refuse more than imagebuilder reads.
func FuzzCheckAgainstImagebuilder(f *testing.F) {
	for _, s := range bypassSeeds() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			t.Skip()
		}
		if msg := differ(data); msg != "" {
			t.Fatal(msg)
		}
	})
}

// TestOracleFindsSeeds: each transport seed is a source that imagebuilder
// finds. Without this, a parser change in imagebuilder that hides the
// source from the oracle makes the fuzz test pass for no reason.
func TestOracleFindsSeeds(t *testing.T) {
	transports := []string{"atomic", "dir", "oci", "tarball"}
	cases := []string{
		"FROM %s:/p\n",
		"FROM alpine\nCOPY --from=%s:/p /a /b\n",
		"FROM alpine\nADD --from=%s:/p /a /b\n",
		"FROM alpine\nRUN --mount=type=bind,from=%s:/p,target=/m true\n",
		"FROM alpine\nONBUILD COPY --from=%s:/p /a /b\n",
		"FROM alpine\nCOPY --from\\\\=%s:/p /a /b\n",
	}
	for _, tr := range transports {
		for _, c := range cases {
			file := fmt.Sprintf(c, tr)
			sources, ok := ImageSources([]byte(file))
			found := false
			for _, s := range sources {
				found = found || Transport(s) == tr
			}
			if !ok || !found {
				t.Errorf("oracle misses the source in %q: %q", file, sources)
			}
		}
	}
}

// TestDifferentialDetectsAdmit: a check that admits everything makes
// differ report a difference, so the fuzz property is not a no-op.
func TestDifferentialDetectsAdmit(t *testing.T) {
	if differ([]byte("FROM tarball:/x\n")) != "" {
		t.Fatal("the check admits a tarball: source")
	}
	sources, _ := ImageSources([]byte("FROM tarball:/x\n"))
	if len(sources) != 1 || Transport(sources[0]) != "tarball" {
		t.Fatalf("sources = %q", sources)
	}
}
