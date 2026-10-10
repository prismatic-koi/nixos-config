package prismcontainer_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

// buildFile runs one build request with the Containerfile content through
// Build and returns the result and the fake podman.
func buildFile(t *testing.T, containerfile string, buildArgs ...string) (prismcontainer.BuildResult, *prismcontainertest.Fake) {
	t.Helper()
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", containerfile)
	f := &prismcontainertest.Fake{}
	res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{BuildArgs: buildArgs})
	return res, f
}

func wantRefused(t *testing.T, res prismcontainer.BuildResult, f *prismcontainertest.Fake, want string) {
	t.Helper()
	if res.ExitCode != prismcontainer.ExitRefused || !strings.HasPrefix(res.Message, "refused: the Containerfile cannot be built") ||
		!strings.Contains(res.Message, want) {
		t.Errorf("result = %+v, want a Containerfile refusal that contains %q", res, want)
	}
	if calls := f.BuildCalls(); len(calls) != 0 {
		t.Errorf("podman build ran: %q", calls)
	}
}

// TestBuild_TransportRefused: each transport that is not a registry is
// refused in FROM, in COPY and ADD --from, and in RUN --mount=from=.
func TestBuild_TransportRefused(t *testing.T) {
	transports := []string{"atomic", "containers-storage", "dir", "docker-archive", "docker-daemon", "oci", "oci-archive", "ostree", "sif", "tarball"}
	places := map[string]string{
		"FROM":               "FROM %s:/home/u/x.tar\n",
		"FROM with platform": "FROM --platform=$BUILDPLATFORM %s:/home/u/x.tar AS base\n",
		"COPY --from=":       "FROM alpine\nCOPY --from=%s:/home/u/x.tar /a /b\n",
		"COPY --from value":  "FROM alpine\nCOPY --from %s:/home/u/x.tar /a /b\n",
		"ADD --from=":        "FROM alpine\nADD --from=%s:/home/u/x.tar /a /b\n",
		"mount from=":        "FROM alpine\nRUN --mount=type=bind,from=%s:/home/u/x.tar,target=/m cat /m/f\n",
		"ONBUILD":            "FROM alpine\nONBUILD COPY --from=%s:/home/u/x.tar /a /b\n",
		"joined line":        "FROM alpine\nRUN \\\n  --mount=type=bind,from=%s:/home/u/x.tar,target=/m true\n",
	}
	for _, tr := range transports {
		for place, format := range places {
			t.Run(tr+"/"+place, func(t *testing.T) {
				res, f := buildFile(t, fmt.Sprintf(format, tr))
				wantRefused(t, res, f, fmt.Sprintf("the %q transport", tr))
			})
		}
	}
}

// TestBuild_NonLiteralReferenceRefused: a reference that is not literal is
// refused, because buildah expands, unquotes, and unescapes it, and the
// result can name a transport. The first four cases are the ARG forms that
// got past the expanding check of an earlier revision.
func TestBuild_NonLiteralReferenceRefused(t *testing.T) {
	cases := map[string]struct {
		file      string
		buildArgs []string
		want      string
	}{
		"quoted ARG word":         {file: "ARG \"B=tarball:/x\"\nFROM $B\n", want: "is not literal"},
		"escaped ARG equals":      {file: "ARG B\\=tarball:/x\nFROM $B\n", want: "is not literal"},
		"ARG name from variable":  {file: "ARG N=B\nARG ${N}=tarball:/x\nFROM $B\n", want: "is not literal"},
		"ARG from variable":       {file: "ARG V=B=tarball:/x\nARG $V\nFROM $B\n", want: "is not literal"},
		"build-arg":               {file: "ARG B\nFROM ${B}\n", buildArgs: []string{"B=tarball:/x"}, want: "is not literal"},
		"variable in a tag":       {file: "ARG GO=1.23\nFROM golang:${GO}\n", want: "Write the image literally"},
		"quoted FROM":             {file: "FROM \"tarball:/x\"\n", want: "is not literal"},
		"backslash in FROM":       {file: "FROM tar\\ball:/x\n", want: "is not literal"},
		"quoted --from value":     {file: "FROM alpine\nCOPY --from=\"tarball:/x\" /a /b\n", want: "is not literal"},
		"backslash --from value":  {file: "FROM alpine\nCOPY --from=tar\\ball:/x /a /b\n", want: "is not literal"},
		"variable --from value":   {file: "FROM alpine\nCOPY --from=$SRC /a /b\n", want: "is not literal"},
		"quoted flag name":        {file: "FROM alpine\nCOPY --fr\"om\"=tarball:/x /a /b\n", want: "name that is not literal"},
		"escaped flag name":       {file: "FROM alpine\nCOPY --fr\\om=tarball:/x /a /b\n", want: "name that is not literal"},
		"quoted mount field":      {file: "FROM alpine\nRUN --mount=type=bind,\"from=tarball:/x\",target=/m true\n", want: "--mount value"},
		"variable mount key":      {file: "FROM alpine\nRUN --mount=type=bind,${K}=tarball:/x,target=/m true\n", want: "--mount value"},
		"variable mount from":     {file: "FROM alpine\nRUN --mount=type=bind,from=$SRC,target=/m true\n", want: "--mount value"},
		"empty --from":            {file: "FROM alpine\nCOPY --from= /a /b\n", want: "is not literal"},
		"non-ASCII in reference":  {file: "FROM alpine\u0130:1\n", want: "is not literal"},
		"invalid UTF-8 in --from": {file: "FROM alpine\nCOPY --from=\xff\xfe /a /b\n", want: "not ASCII"},
		"escape directive":        {file: "# escape=`\nFROM alpine\n", want: "escape directive"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res, f := buildFile(t, tc.file, tc.buildArgs...)
			wantRefused(t, res, f, tc.want)
		})
	}
}

// TestBuild_ParserSplitRefused: the Dockerfile parser reads the flags of a
// line byte by byte, so the bytes 0x85 and 0xA0 split a flag word, also
// inside a UTF-8 character (U+0160 is C5 A0, U+0105 is C4 85). It also
// lower-cases the keyword with strings.ToLower, so "ONBUİLD" is ONBUILD.
// Each of these lines gives buildah a tarball: source.
func TestBuild_ParserSplitRefused(t *testing.T) {
	cases := map[string]struct{ file, want string }{
		"COPY --exclude then --from": {"FROM alpine\nCOPY --exclude=x\u0160--from=tarball:/p /a /b\n", "not ASCII"},
		"COPY --chown then --from":   {"FROM alpine\nCOPY --chown=0\u0105--from=tarball:/p /a /b\n", "not ASCII"},
		"RUN --network then --mount": {"FROM alpine\nRUN --network=none\u0160--mount=type=bind,from=tarball:/p,target=/m true\n", "not ASCII"},
		"FROM --platform then image": {"FROM --platform=linux/amd64\u0160tarball:/p\n", "not ASCII"},
		"lone 0x85 before --from":    {"FROM alpine\nCOPY \x85--from=tarball:/p /a /b\n", `the "tarball" transport`},
		"lone 0xA0 before --mount":   {"FROM alpine\nRUN \xa0--mount=type=bind,from=tarball:/p,target=/m true\n", `the "tarball" transport`},
		"dotted I in ONBUILD":        {"FROM alpine\nONBU\u0130LD COPY --from=tarball:/p /a /b\n", `the "tarball" transport`},
		// review-security round 9: the word view ends its flag zone early,
		// and ProcessWord turns the next flag into --from or --mount.
		"zone desync, escaped from":  {"FROM alpine\nCOPY --exclude='\\'\\' z' --from\\\\=tarball:/x /etc/passwd /out\n", "name that is not literal"},
		"zone desync, escaped mount": {"FROM alpine\nRUN --network='\\'\\' z' --mou\\\\nt=type=bind,from=tarball:/x,target=/t true\n", "name that is not literal"},
		"zone desync, variable from": {"FROM alpine\nCOPY --exclude='\\'\\' z' --from$E=tarball:/x /a /b\n", "name that is not literal"},
		// review-qa round 9: a flag after a lone 0x85 or 0xA0 byte, which
		// only the parser view sees.
		"lone 0xA0, variable in mount": {"FROM alpine\nRUN \xa0--mo${X}unt=type=bind,from=tarball:/home/u/x.tar,target=/m cat /m/f\n", "name that is not literal"},
		"lone 0xA0, quotes in mount":   {"FROM alpine\nRUN \xa0--mou'\"nt\"'=type=bind,from=tarball:/home/u/x.tar,target=/m cat /m/f\n", "name that is not literal"},
		"lone 0x85, escape in mount":   {"FROM alpine\nRUN \x85--mou\\\\nt=type=bind,from=tarball:/home/u/x.tar,target=/m cat /m/f\n", "name that is not literal"},
		"lone 0xA0, variable in from":  {"FROM alpine\nCOPY \xa0--fr${X}om=tarball:/home/u/x.tar /a /b\n", "name that is not literal"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res, f := buildFile(t, tc.file)
			wantRefused(t, res, f, tc.want)
		})
	}
}

// TestBuild_PhysicalLineChecked: every physical line gets the full check,
// so a line that prism joins and buildah does not cannot hide an image
// source. The parser does not join a heredoc terminator, and it removes
// leading unicode.IsSpace characters before it reads a directive.
func TestBuild_PhysicalLineChecked(t *testing.T) {
	const arg = "ARG X=tarball:/home/u/x.tar\nFROM alpine\n"
	const heredoc = "RUN <<'T\\'\necho hi \\\nT\\\n"
	cases := map[string]struct {
		file string
		line int
		want string
	}{
		"heredoc terminator, FROM":        {arg + heredoc + "FROM $X\n", 6, "is not literal"},
		"heredoc terminator, COPY":        {arg + heredoc + "COPY --from=$X / /x\n", 6, "is not literal"},
		"heredoc terminator, mount":       {arg + heredoc + "RUN --mount=type=bind,from=$X,target=/m cat /m/f\n", 6, "--mount value"},
		"dash heredoc terminator, FROM":   {arg + "RUN <<-'T\\'\necho hi\nT\\\nFROM \"$X\" AS y\n", 6, "is not literal"},
		"vertical tab before FROM":        {arg + "\vFROM $X\n", 3, "is not literal"},
		"vertical tab before escape":      {"\v# escape=`\n" + arg + "RUN echo a b \\\nFROM $X\n", 1, "escape directive"},
		"no-break space before escape":    {"\u00a0# escape=`\nFROM alpine\n", 1, "escape directive"},
		"ideographic space before escape": {"\u3000# escape=`\nFROM alpine\n", 1, "escape directive"},
		"SQL text in a RUN continuation":  {"FROM postgres:16\nRUN psql -c \"SELECT * \\\nFROM users\"\n", 3, "restructure the text"},
		"heredoc terminator, split FROM":  {arg + heredoc + "FR\\\nOM $X\n", 6, "is not literal"},
		"heredoc terminator, split flag":  {arg + heredoc + "COPY \\\n--fr\\\nom=$X / /\n", 6, "is not literal"},
		"heredoc terminator, split value": {arg + heredoc + "COPY --from=tar\\\nball:/x / /\n", 6, `the "tarball" transport`},
		"continuation over 200 lines":     {"FROM alpine\nRUN true" + strings.Repeat(" \\\n  && true", 200) + "\n", 2, "more than 200 lines"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res, f := buildFile(t, tc.file)
			wantRefused(t, res, f, tc.want)
			if want := fmt.Sprintf("line %d:", tc.line); !strings.Contains(res.Message, want) {
				t.Errorf("message = %q, want it to name %s", res.Message, want)
			}
		})
	}
}

// TestBuild_LiteralReferencesPass: literal references, stage names, stage
// indexes, and the usual flags and shell code are not refused.
func TestBuild_LiteralReferencesPass(t *testing.T) {
	cases := map[string]string{
		"registry image":        "FROM docker.io/library/alpine:3.20\n",
		"docker://":             "FROM docker://alpine\n",
		"docker image":          "FROM docker:24-dind\n",
		"registry with a port":  "FROM localhost:5000/team/app:1\n",
		"digest":                "FROM alpine@sha256:" + strings.Repeat("a", 64) + "\n",
		"scratch":               "FROM scratch\nCOPY . /\n",
		"stage name":            "FROM --platform=$BUILDPLATFORM golang:1.23 AS build\nRUN go build -o /app .\nFROM alpine\nCOPY --from=build /app /app\n",
		"stage index":           "FROM golang:1.23\nRUN true\nFROM alpine\nCOPY --from=0 /x /x\n",
		"mount from a stage":    "FROM alpine AS src\nFROM alpine\nRUN --mount=type=bind,from=src,source=/etc,target=/m ls /m\n",
		"cache mount":           "FROM golang:1.23\nRUN --mount=type=cache,target=/root/.cache/go-build go build ./...\n",
		"chown with variables":  "FROM alpine\nARG UID=1000\nCOPY --chown=${UID}:${UID} . /src\n",
		"shell code":            "FROM alpine\nRUN for f in $(ls /etc); do echo \"${f#x}\" $1; done\n",
		"heredoc":               "FROM alpine\nRUN <<EOF\necho 'it'\\''s here'\nEOF\n",
		"escape default":        "# escape=\\\nFROM alpine\n",
		"ENV and ARG":           "FROM alpine\nENV PATH=/opt/bin:$PATH\nARG A=\"x y\"\n",
		"shell flags on joined": "FROM alpine\nRUN kubectl create secret generic s \\\n  --from-literal=k=$V \\\n  && rsync -a \\\n  --exclude-from=$LIST /a /b\n",
		"comment with a slash":  "# a comment \\\nFROM alpine\n",
		"split literal FROM":    "FROM \\\n  --platform=$BUILDPLATFORM \\\n  golang:1.23 \\\n  AS build\nRUN true\n",
		"shell flags in a RUN":  "FROM alpine\nRUN cmd \\\n  -- arg \\\n  --some_flag=1 \\\n  --from-file=$X \\\n  --mount=$M\n",
		"continuation of 199":   "FROM alpine\nRUN true" + strings.Repeat(" \\\n  && true", 198) + "\n",
		"heredoc Python import": "FROM python:3.12\nRUN <<EOF python3\nfrom typing import List, Dict\nfrom foo import *\nEOF\n",
		"heredoc SQL":           "FROM postgres:16\nCOPY <<EOF /init.sql\nSELECT name\nFROM users WHERE id = 1;\nEOF\n",
		"non-ASCII in RUN":      "FROM alpine\nRUN echo caf\u00e9 \u0160\n",
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			res, _ := buildFile(t, file)
			if res.ExitCode != 0 {
				t.Errorf("result = %+v, want the build to run", res)
			}
		})
	}
}

// TestBuild_TransportRefusedOnAnyLine: a transport reference is refused on
// a line that the joins do not present as an instruction, and in odd
// whitespace and encodings.
func TestBuild_TransportRefusedOnAnyLine(t *testing.T) {
	cases := map[string]string{
		"comment that ends in a backslash": "# a comment \\\nFROM tarball:/x\n",
		"byte order mark":                  "\ufeffFROM tarball:/x\n",
		"form feed and vertical tab":       "\fFROM\vtarball:/x\n",
		"CRLF":                             "FROM alpine\r\nCOPY --from=tarball:/x /a /b\r\n",
		"lower case":                       "from tarball:/x\n",
		"upper case flag":                  "FROM alpine\nCOPY --FROM=tarball:/x /a /b\n",
		"continuation with comment":        "FROM \\\n# a comment\n  tarball:/x\n",
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			res, f := buildFile(t, file)
			wantRefused(t, res, f, `the "tarball" transport`)
		})
	}
}

// TestBuild_PolicyRefusalNamed: when the signature policy of a Linux
// build refuses a source that the check cannot see (an ONBUILD
// instruction of a base image), the result names the cause.
func TestBuild_PolicyRefusalNamed(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM localhost/base-with-onbuild\n")
	f := &prismcontainertest.Fake{}
	f.OnBuild = func(_ context.Context, out io.Writer, _ []string) (int, error) {
		_, _ = out.Write([]byte("Error: creating build container: Source image rejected: Running image tarball:/home/u/x.tar is rejected by policy.\n"))
		return 125, nil
	}
	res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{})
	if res.ExitCode != 125 || !strings.Contains(res.Message, "signature policy of the build refused an image source") ||
		!strings.Contains(res.Message, "ONBUILD") {
		t.Errorf("result = %+v, want the policy refusal named", res)
	}
}

// TestBuild_InFileRefused: podman runs a Containerfile whose name ends in
// ".in" through cpp on the host, so prism refuses the name.
func TestBuild_InFileRefused(t *testing.T) {
	for _, name := range []string{"Containerfile.in", "build/app.IN"} {
		t.Run(name, func(t *testing.T) {
			c := newCaller(t)
			writeFile(t, c.Worktree, name, "COPY <<EOF /s\n#include \"/etc/passwd\"\nEOF\n")
			f := &prismcontainertest.Fake{}
			res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{File: name})
			if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "C preprocessor") {
				t.Errorf("result = %+v, want a refusal that names the preprocessor", res)
			}
			if len(f.BuildCalls()) != 0 {
				t.Errorf("podman build ran")
			}
		})
	}
}
