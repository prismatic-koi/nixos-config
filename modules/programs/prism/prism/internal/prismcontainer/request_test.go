package prismcontainer

import (
	"strings"
	"testing"
	"time"
)

func TestNormaliseImage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"alpine", "docker.io/library/alpine"},
		{"alpine:3.20", "docker.io/library/alpine:3.20"},
		{"library/alpine", "docker.io/library/alpine"},
		{"bitnami/postgresql:16", "docker.io/bitnami/postgresql:16"},
		{"docker.io/library/alpine", "docker.io/library/alpine"},
		{"quay.io/podman/stable:latest", "quay.io/podman/stable:latest"},
		{"ghcr.io/org/repo/img@sha256:" + strings.Repeat("a", 64), "ghcr.io/org/repo/img@sha256:" + strings.Repeat("a", 64)},
		{"localhost/prism-built:1", "localhost/prism-built:1"},
		{"localhost:5000/img", "localhost:5000/img"},
		{"3f57d9401f8d", "3f57d9401f8d"},
	}
	for _, tc := range cases {
		got, err := normaliseImage(tc.in)
		if err != nil {
			t.Errorf("normaliseImage(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normaliseImage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormaliseImage_Refused(t *testing.T) {
	cases := map[string]string{
		"":                                   "empty",
		"-v":                                 `starts with "-"`,
		"--privileged":                       `starts with "-"`,
		"-":                                  `starts with "-"`,
		"Alpine":                             "not a valid",
		"alpine latest":                      "not a valid",
		"alpine\n--privileged":               "not a valid",
		"docker-archive:/etc/shadow":         "not a valid",
		"oci-archive:/home/u/x.tar":          "not a valid",
		"docker://alpine":                    "not a valid",
		"oci:1234/x":                         "transport",
		"dir:5000/x":                         "transport",
		"containers-storage:1/x":             "transport",
		strings.Repeat("a", 600):             "longer than",
		"alpine:" + strings.Repeat("t", 200): "not a valid",
	}
	for in, want := range cases {
		_, err := normaliseImage(in)
		if err == nil {
			t.Errorf("normaliseImage(%q): want error containing %q, got nil", in, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("normaliseImage(%q): error %q does not contain %q", in, err, want)
		}
	}
}

func TestValidate_Defaults(t *testing.T) {
	v, err := RunRequest{Image: "alpine"}.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if v.mount != MountRO {
		t.Errorf("default mount = %q, want %q", v.mount, MountRO)
	}
	if v.timeout != 10*time.Minute {
		t.Errorf("default timeout = %s, want 10m", v.timeout)
	}
}

func TestValidate_Timeout(t *testing.T) {
	if _, err := (RunRequest{Image: "alpine", TimeoutSeconds: 3600}).validate(); err != nil {
		t.Errorf("60m timeout refused: %v", err)
	}
	_, err := RunRequest{Image: "alpine", TimeoutSeconds: 3601}.validate()
	if err == nil || !strings.Contains(err.Error(), "maximum of 1h0m0s") {
		t.Errorf("timeout over 60m: err = %v, want refusal naming the maximum", err)
	}
	if _, err := (RunRequest{Image: "alpine", TimeoutSeconds: -5}).validate(); err == nil {
		t.Errorf("negative timeout accepted")
	}
}

func TestValidate_Mount(t *testing.T) {
	for _, m := range MountModes {
		if _, err := (RunRequest{Image: "alpine", Mount: Mount(m)}).validate(); err != nil {
			t.Errorf("mount %q refused: %v", m, err)
		}
	}
	if _, err := (RunRequest{Image: "alpine", Mount: "/etc"}).validate(); err == nil {
		t.Errorf("mount /etc accepted")
	}
}

func TestValidate_Env(t *testing.T) {
	v, err := RunRequest{Image: "alpine", Env: []string{"A=1", "_b2=x=y", "EMPTY="}}.validate()
	if err != nil {
		t.Fatalf("valid env refused: %v", err)
	}
	if strings.Join(v.envKeys, ",") != "A,_b2,EMPTY" {
		t.Errorf("envKeys = %v", v.envKeys)
	}

	refused := []string{
		"HOME",         // no "=": podman would copy the host value
		"GITHUB_*",     // no "=": podman would copy every matching host value
		"1A=x",         // starts with a digit
		"A-B=x",        // dash
		"=x",           // empty key
		"A B=x",        // space
		"--privileged", // no "=" and a flag shape
	}
	for _, e := range refused {
		if _, err := (RunRequest{Image: "alpine", Env: []string{e}}).validate(); err == nil {
			t.Errorf("env %q accepted, want refusal", e)
		}
	}
}
