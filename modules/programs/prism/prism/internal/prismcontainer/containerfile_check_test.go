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
// refused in FROM, in COPY --from=, and in RUN --mount=from=.
func TestBuild_TransportRefused(t *testing.T) {
	transports := []string{"atomic", "containers-storage", "dir", "docker-archive", "docker-daemon", "oci", "oci-archive", "ostree", "sif", "tarball"}
	places := map[string]string{
		"FROM":        "FROM %s:/home/u/x.tar\n",
		"COPY --from": "FROM alpine\nCOPY --from=%s:/home/u/x.tar /a /b\n",
		"mount from":  "FROM alpine\nRUN --mount=type=bind,from=%s:/home/u/x.tar,target=/m cat /m/f\n",
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

// TestBuild_TransportRefusedThroughExpansion: each expansion form that the
// check reads can build a transport reference, and the check refuses it.
func TestBuild_TransportRefusedThroughExpansion(t *testing.T) {
	cases := []struct {
		name      string
		file      string
		buildArgs []string
	}{
		{"$VAR", "ARG B=tarball:/x\nFROM $B\n", nil},
		{"${VAR}", "ARG B=tarball:/x\nFROM ${B}\n", nil},
		{"${VAR:-word}", "FROM ${UNSET:-tarball:/x}\n", nil},
		{"${VAR-word}", "FROM ${UNSET-tarball:/x}\n", nil},
		{"${VAR:+word}", "ARG S=1\nFROM ${S:+tarball:/x}\n", nil},
		{"${VAR+word}", "ARG S=1\nFROM ${S+tarball:/x}\n", nil},
		{"build-arg", "ARG B\nFROM $B\n", []string{"B=tarball:/x"}},
		{"build-arg override", "ARG B=alpine\nFROM $B\n", []string{"B=tarball:/x"}},
		{"joined variables", "ARG A=tar\nARG C=ball\nFROM ${A}${C}:/x\n", nil},
		{"nested ARG", "ARG A=tarball\nARG B=${A}:/x\nFROM $B\n", nil},
		{"nested default", "ARG A=tarball\nFROM ${UNSET:-${A}:/x}\n", nil},
		{"platform arg", "FROM tarball${TARGETVARIANT}:/x\n", nil},
		{"double quotes", "FROM \"tarball:/x\"\n", nil},
		{"single quotes", "FROM 'tar'ball:/x\n", nil},
		{"backslash", "FROM tar\\ball:/x\n", nil},
		{"lower case", "from tarball:/x\n", nil},
		{"platform flag first", "FROM --platform=linux/amd64 tarball:/x AS base\n", nil},
		{"continuation", "FROM \\\n  tarball:/x\n", nil},
		{"continuation with comment", "FROM \\\n# a comment\n  tarball:/x\n", nil},
		{"ONBUILD", "FROM alpine\nONBUILD COPY --from=tarball:/x /a /b\n", nil},
		{"--from with a space", "FROM alpine\nCOPY --from tarball:/x /a /b\n", nil},
		{"--FROM upper case", "FROM alpine\nCOPY --FROM=tarball:/x /a /b\n", nil},
		{"quoted mount", "FROM alpine\nRUN --mount=type=bind,\"from=tarball:/x\",target=/m true\n", nil},
		{"mount on a joined line", "FROM alpine\nRUN \\\n  --mount=type=bind,from=tarball:/x,target=/m true\n", nil},
		{"CRLF", "FROM alpine\r\nCOPY --from=tarball:/x /a /b\r\n", nil},
		{"byte order mark", "\ufeffFROM tarball:/x\n", nil},
		{"form feed", "\fFROM\vtarball:/x\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, f := buildFile(t, tc.file, tc.buildArgs...)
			wantRefused(t, res, f, `the "tarball" transport`)
		})
	}
}

// TestBuild_UncheckableFormsRefused: a form that the check cannot expand
// is refused, because buildah can expand it to a transport reference.
func TestBuild_UncheckableFormsRefused(t *testing.T) {
	cases := map[string]struct {
		file string
		want string
	}{
		"${VAR#pattern}":           {"ARG B=xtarball:/x\nFROM ${B#x}\n", "is not supported"},
		"${VAR:?word}":             {"FROM ${B:?missing}\n", "is not supported"},
		"${VAR/a/b}":               {"ARG B=x:/x\nFROM ${B/x/tarball}\n", "is not supported"},
		"${#VAR}":                  {"FROM ${#B}\n", "is not supported"},
		"command substitution":     {"FROM $(echo tarball:/x)\n", "not followed by a variable name"},
		"$1":                       {"FROM $1tarball:/x\n", "not followed by a variable name"},
		"open quote":               {"FROM \"tarball:/x\n", "quote is not closed"},
		"unexpandable ARG":         {"ARG B=$(id)\nFROM $B\n", "cannot check"},
		"ARG loop":                 {"ARG A=${B}\nARG B=${A}\nFROM $A\n", "cannot check"},
		"variable in --from":       {"FROM alpine\nCOPY --from=$SRC /a /b\n", "cannot check the flag"},
		"variable in mount":        {"FROM alpine\nRUN --mount=type=bind,from=${SRC},target=/m true\n", "cannot check the flag"},
		"variable in --from value": {"FROM alpine\nCOPY --from ${SRC} /a /b\n", "cannot check the --from value"},
		"escape directive":         {"# escape=`\nFROM alpine\n", "escape directive"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res, f := buildFile(t, tc.file)
			wantRefused(t, res, f, tc.want)
		})
	}
}

// TestBuild_CommonContainerfilesPass: usual Containerfiles are not refused.
func TestBuild_CommonContainerfilesPass(t *testing.T) {
	cases := map[string]string{
		"docker://":            "FROM docker://alpine\n",
		"docker image":         "FROM docker:24-dind\n",
		"registry with a port": "FROM localhost:5000/team/app:1\n",
		"ARG in tag":           "ARG GO_VERSION=1.23\nFROM golang:${GO_VERSION} AS build\nRUN go version\n",
		"multi-stage":          "FROM --platform=$BUILDPLATFORM golang:1.23 AS build\nRUN go build -o /app .\nFROM alpine\nCOPY --from=build /app /app\n",
		"cache mount":          "FROM golang:1.23\nRUN --mount=type=cache,target=/root/.cache/go-build go build ./...\n",
		"chown with variables": "FROM alpine\nARG UID=1000\nCOPY --chown=${UID}:${UID} . /src\n",
		"shell code":           "FROM alpine\nRUN for f in $(ls /etc); do echo \"${f#x}\" $1; done\n",
		"heredoc":              "FROM alpine\nRUN <<EOF\necho 'it'\\''s here'\nEOF\n",
		"escape default":       "# escape=\\\nFROM alpine\n",
		"default value":        "FROM ${BASE:-alpine}\n",
		"ENV and ARG":          "FROM alpine\nENV PATH=/opt/bin:$PATH\nARG A=\"x y\"\n",
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
