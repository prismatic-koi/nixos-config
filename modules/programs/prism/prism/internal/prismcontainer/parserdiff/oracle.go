// Package parserdiff is a differential fuzz test of the Containerfile check
// of prism container build against the Dockerfile parser of imagebuilder.
//
// It is a separate Go module, so that the main prism module, its vendor
// hash, and "go test ./..." do not hold the imagebuilder dependency. The
// imagebuilder version is the one that the host buildah uses (v1.2.19 for
// buildah v1.43.4). Change it with the buildah version.
package parserdiff

import (
	"strings"

	"github.com/openshift/imagebuilder"
	"github.com/openshift/imagebuilder/dockerfile/parser"
)

// refusedTransports are the transports other than docker:// that libimage
// reads from a path on the host or from a local store.
var refusedTransports = []string{
	"atomic", "containers-storage", "dir", "docker-archive", "docker-daemon",
	"oci", "oci-archive", "ostree", "sif", "tarball",
}

// fuzzEnv gives the common variable names of the seed corpus a transport
// value, so that an expansion that reaches a source shows as a transport.
var fuzzEnv = []string{
	"X=tarball:/x", "E=tarball:/x", "B=tarball:/x", "V=tarball:/x",
	"N=tarball:/x", "K=from", "M=type=bind,from=tarball:/x", "SRC=tarball:/x",
}

// ImageSources returns every image source that imagebuilder finds in the
// Containerfile: the image of FROM, the value of --from of COPY and ADD, and
// each from= value of --mount of RUN. Variables are expanded. ok is false
// when imagebuilder does not parse the file, and then buildah builds nothing.
func ImageSources(data []byte) (sources []string, ok bool) {
	res, err := parser.Parse(strings.NewReader(string(data)))
	if err != nil || res.AST == nil {
		return nil, false
	}
	env := append([]string(nil), fuzzEnv...)
	var walk func(n *parser.Node)
	walk = func(n *parser.Node) {
		for _, c := range n.Children {
			collect(c, &env, &sources)
			walk(c)
			if c.Next != nil {
				walk(c.Next) // ONBUILD holds its instruction here.
			}
		}
	}
	walk(res.AST)
	return sources, true
}

func collect(n *parser.Node, env *[]string, out *[]string) {
	cmd := strings.ToLower(n.Value)
	expand := func(s string) string {
		if v, err := imagebuilder.ProcessWord(s, *env); err == nil {
			return v
		}
		return s
	}
	switch cmd {
	case "arg", "env":
		for a := n.Next; a != nil; a = a.Next {
			if k, v, found := strings.Cut(a.Value, "="); found {
				*env = append(*env, k+"="+expand(v))
			}
		}
	case "from":
		if n.Next != nil {
			*out = append(*out, expand(n.Next.Value))
		}
	case "copy", "add":
		for _, f := range n.Flags {
			if name, v, _ := strings.Cut(expand(f), "="); strings.EqualFold(name, "--from") {
				*out = append(*out, v)
			}
		}
	case "run":
		for _, f := range n.Flags {
			name, v, _ := strings.Cut(expand(f), "=")
			if !strings.EqualFold(name, "--mount") {
				continue
			}
			for _, field := range strings.Split(v, ",") {
				if k, ref, found := strings.Cut(field, "="); found && strings.EqualFold(strings.TrimSpace(k), "from") {
					*out = append(*out, ref)
				}
			}
		}
	}
}

// Transport returns the refused transport that a source uses, or "".
func Transport(source string) string {
	for _, t := range refusedTransports {
		if strings.HasPrefix(source, t+":") {
			return t
		}
	}
	return ""
}
