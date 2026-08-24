package main

import (
	"net/url"
	"path/filepath"
	"testing"
)

// A resource URI has to survive being parsed, because the MCP SDK parses every
// one it is given and panics on one it cannot — so a URI this server cannot
// build correctly is not a missing document, it is a server that dies before
// it answers. The paths come from t.TempDir so the platform's own separator is
// what is exercised: on Windows that is the drive letter and the backslashes
// that used to make this "file://D:\..." and unparseable.
func TestAServedPathSurvivesBecomingAURIAndComingBack(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"dictionary.md", "tariff catalog.md", "a+b&c.md"} {
		path := filepath.Join(dir, name)
		uri := fileURI(path)

		if _, err := url.Parse(uri); err != nil {
			t.Errorf("%q became %q, which does not parse: %v", path, uri, err)
			continue
		}
		got, ok := filePath(uri)
		if !ok {
			t.Errorf("%q became %q, which did not read back as a path", path, uri)
			continue
		}
		if got != path {
			t.Errorf("%q became %q and came back as %q", path, uri, got)
		}
	}
}

// Only the URIs this server advertises, which are local files.
func TestOnlyALocalFileURLNamesAPath(t *testing.T) {
	for _, uri := range []string{
		"https://example.com/dictionary.md",
		"file://elsewhere/dictionary.md",
		"docs:dictionary",
		"::not a uri",
	} {
		if got, ok := filePath(uri); ok {
			t.Errorf("%q was read as the path %q", uri, got)
		}
	}
}
