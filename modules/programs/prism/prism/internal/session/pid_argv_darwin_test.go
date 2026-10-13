package session

import (
	"encoding/binary"
	"slices"
	"testing"
)

func procArgs2Buf(argc int32, body string) []byte {
	buf := binary.NativeEndian.AppendUint32(nil, uint32(argc))
	return append(buf, body...)
}

func TestParseProcArgs2(t *testing.T) {
	cases := []struct {
		desc    string
		buf     []byte
		want    []string
		wantErr bool
	}{
		{"arguments after exec path and padding", procArgs2Buf(3, "/bin/prism\x00\x00\x00\x00prism\x00sidecar\x00--session\x00HOME=/x\x00"), []string{"prism", "sidecar", "--session"}, false},
		{"no padding", procArgs2Buf(2, "/bin/sleep\x00sleep\x0030\x00"), []string{"sleep", "30"}, false},
		{"argc zero", procArgs2Buf(0, "/bin/x\x00\x00"), []string{}, false},
		{"buffer without argc", []byte{1, 0}, nil, true},
		{"exec path without terminator", procArgs2Buf(1, "/bin/x"), nil, true},
		{"fewer arguments than argc", procArgs2Buf(3, "/bin/x\x00\x00x\x00y\x00"), nil, true},
		{"negative argc", procArgs2Buf(-1, "/bin/x\x00"), nil, true},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got, err := parseProcArgs2(c.buf)
			if (err != nil) != c.wantErr {
				t.Fatalf("parseProcArgs2 error = %v, want error %v", err, c.wantErr)
			}
			if !c.wantErr && !slices.Equal(got, c.want) {
				t.Errorf("parseProcArgs2 = %q, want %q", got, c.want)
			}
		})
	}
}
