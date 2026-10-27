package container

// pi_resume_cwd_parse_test.go — CheckPIResumeCwd fails closed when the pi
// parsers and the Go parser can read different headers (review of PR #3294).

import (
	"strings"
	"testing"
)

// A session header with no string id makes the list path of pi fail, so
// the check refuses it.
func TestCheckPIResumeCwd_RefusesHeaderWithoutStringID(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl", piSessionHeaderLine(t, resumeCwdID, resumeCwdWorktree))
	writeSessionFile(t, dir, "x.jsonl", []byte(`{"type":"session","id":7,"cwd":"/tmp"}`+"\n"))

	if err := CheckPIResumeCwd(resumeCwdCfg()); err == nil {
		t.Fatal("CheckPIResumeCwd = nil for a header with a number id, want an error")
	}
}

// A file whose first line is another entry type, and an empty file, have no
// header for any pi parser. The check skips them.
func TestCheckPIResumeCwd_SkipsNonSessionAndEmptyFiles(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl", piSessionHeaderLine(t, resumeCwdID, resumeCwdWorktree))
	writeSessionFile(t, dir, "message.jsonl", []byte(`{"type":"message","id":"`+resumeCwdID+`x","cwd":"/tmp"}`+"\n"))
	writeSessionFile(t, dir, "empty.jsonl", nil)

	if err := CheckPIResumeCwd(resumeCwdCfg()); err != nil {
		t.Fatalf("CheckPIResumeCwd = %v, want nil", err)
	}
}

// A header line of a few kB, longer than the default bufio buffer, passes.
func TestCheckPIResumeCwd_AcceptsLongHeaderLine(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	line := `{"type":"session","id":"` + resumeCwdID + `","cwd":"` + resumeCwdWorktree + `","pad":"` + strings.Repeat("a", 10000) + `"}` + "\n"
	writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl", []byte(line))

	if err := CheckPIResumeCwd(resumeCwdCfg()); err != nil {
		t.Fatalf("CheckPIResumeCwd = %v, want nil", err)
	}
}

// The repro files from the review of PR #3294. In each case, Go and pi read
// different headers from the same file, or pi reads a header that the old
// check did not read. Each case must stop the resume.
func TestCheckPIResumeCwd_RefusesParserMismatchFiles(t *testing.T) {
	good := piSessionHeaderLine(t, resumeCwdID, resumeCwdWorktree)
	goodName := "2026-01-02T03-04-05-000Z_" + resumeCwdID + ".jsonl"
	deep := strings.Repeat("[", 10001) + strings.Repeat("]", 10001)
	cases := map[string]map[string][]byte{
		// review-code: pi reads `true` as a non-session entry, and the old
		// check skipped it. The second file has a number that overflows
		// float64 in Go; JSON.parse gives Infinity.
		"true line and 1e400": {
			goodName:  append([]byte("true\n"), good...),
			"x.jsonl": []byte(`{"type":"session","id":"` + resumeCwdID + `X","cwd":"/tmp","x":1e400}` + "\n"),
		},
		// review-code: nesting deeper than the limit of the Go decoder.
		"deep nesting": {
			goodName:  good,
			"d.jsonl": []byte(`{"type":"session","id":"` + resumeCwdID + `X","cwd":"/tmp","x":` + deep + "}\n"),
		},
		// review-security: a first line that is not an object, and a "\r"
		// that the list path of pi splits on.
		"number line and carriage return": {
			"a.jsonl": append([]byte("1\n"), good...),
			"b.jsonl": []byte(`{"type":"session","id":"` + resumeCwdID + `x","cwd":"` + resumeCwdWorktree + `"}` + "\r{}\n" +
				`{"type":"session","id":"zz","cwd":"/evil"}` + "\n"),
		},
		// review-qa: a first line that pi accepts and Go rejects, then a
		// good header.
		"1e400 header then good header": {
			goodName: append([]byte(`{"type":"session","id":"`+resumeCwdID+`","cwd":"/tmp/evil","x":1e400}`+"\n"), good...),
		},
		// review-qa: headers past the 1 MiB findById scan of pi. The list
		// path of pi still reads them.
		"large blank prefix": {
			"a.jsonl": append([]byte(strings.Repeat("\n", 2<<20)), good...),
			"b.jsonl": append([]byte(strings.Repeat("\n", 9<<20)),
				[]byte(`{"type":"session","id":"`+resumeCwdID+`-zz","cwd":"/tmp/evil","timestamp":"2099-01-01T00:00:00.000Z"}`+"\n")...),
		},
		"first line too long": {
			goodName: good,
			"l.jsonl": []byte(`{"type":"session","id":"` + resumeCwdID + `x","cwd":"` + resumeCwdWorktree +
				`","pad":"` + strings.Repeat("a", piHeaderLineLimit) + `"}` + "\n"),
		},
		"byte order mark": {
			goodName:  good,
			"m.jsonl": append([]byte("\xef\xbb\xbf"), good...),
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			dir := resumeCwdFixture(t, resumeCwdWorktree)
			for fname, content := range files {
				writeSessionFile(t, dir, fname, content)
			}
			if err := CheckPIResumeCwd(resumeCwdCfg()); err == nil {
				t.Fatal("CheckPIResumeCwd = nil, want an error")
			}
		})
	}
}
