package search

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	"github.com/sourcegraph/sourcegraph/cmd/searcher/protocol"
	"github.com/sourcegraph/sourcegraph/internal/comby"
	"github.com/sourcegraph/sourcegraph/internal/search"
	storetest "github.com/sourcegraph/sourcegraph/internal/store/testutil"
)

func TestMatcherLookupByLanguage(t *testing.T) {
	// If we are not on CI skip the test.
	if os.Getenv("CI") == "" {
		t.Skip("Not on CI, skipping comby-dependent test")
	}

	input := map[string]string{
		"file_without_extension": `
/* This foo(plain string) {} is in a Go comment should not match in Go, but should match in plaintext */
func foo(go string) {}
`,
	}

	cases := []struct {
		Name      string
		Languages []string
		Want      []string
	}{
		{
			Name:      "Language test for no language",
			Languages: []string{},
			Want:      []string{"foo(plain string)", "foo(go string)"},
		},
		{
			Name:      "Language test for Go",
			Languages: []string{"go"},
			Want:      []string{"foo(go string)"},
		},
		{
			Name:      "Language test for plaintext",
			Languages: []string{"text"},
			Want:      []string{"foo(plain string)", "foo(go string)"},
		},
	}

	zipData, err := storetest.CreateZip(input)
	if err != nil {
		t.Fatal(err)
	}
	zf, cleanup, err := storetest.TempZipFileOnDisk(zipData)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	t.Run("group", func(t *testing.T) {
		for _, tt := range cases {
			tt := tt
			t.Run(tt.Name, func(t *testing.T) {
				t.Parallel()

				p := &protocol.PatternInfo{
					Pattern:         "foo(:[args])",
					IncludePatterns: []string{"file_without_extension"},
					Languages:       tt.Languages,
				}

				ctx, cancel, sender := newLimitedStreamCollector(context.Background(), 100000000)
				defer cancel()
				err := structuralSearch(ctx, zf, Subset(p.IncludePatterns), "", p.Pattern, p.CombyRule, p.Languages, "repo_foo", sender)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, fileMatches := range sender.collected {
					for _, m := range fileMatches.LineMatches {
						got = append(got, m.Preview)
					}
				}

				if !reflect.DeepEqual(got, tt.Want) {
					t.Fatalf("got file matches %v, want %v", got, tt.Want)
				}
			})
		}
	})
}

func TestMatcherLookupByExtension(t *testing.T) {
	// If we are not on CI skip the test.
	if os.Getenv("CI") == "" {
		t.Skip("Not on CI, skipping comby-dependent test")
	}

	t.Parallel()

	input := map[string]string{
		"file_without_extension": `
/* This foo(plain.empty) {} is in a Go comment should not match in Go, but should match in plaintext */
func foo(go.empty) {}
`,
		"file.go": `
/* This foo(plain.go) {} is in a Go comment should not match in Go, but should match in plaintext */
func foo(go.go) {}
`,
		"file.txt": `
/* This foo(plain.txt) {} is in a Go comment should not match in Go, but should match in plaintext */
func foo(go.txt) {}
`,
	}

	zipData, err := storetest.CreateZip(input)
	if err != nil {
		t.Fatal(err)
	}
	zf, cleanup, err := storetest.TempZipFileOnDisk(zipData)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	test := func(language, filename string) string {
		var languages []string
		if language != "" {
			languages = []string{language}
		}

		extensionHint := filepath.Ext(filename)
		ctx, cancel, sender := newLimitedStreamCollector(context.Background(), 1000000000)
		defer cancel()
		err := structuralSearch(ctx, zf, All, extensionHint, "foo(:[args])", "", languages, "repo_foo", sender)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		var got []string
		for _, fileMatches := range sender.collected {
			for _, m := range fileMatches.LineMatches {
				got = append(got, m.Preview)
			}
		}
		sort.Strings(got)
		return strings.Join(got, " ")
	}

	cases := []struct {
		name     string
		want     string
		language string
		filename string
	}{{
		name:     "No language and no file extension => .generic matcher",
		want:     "foo(go.empty) foo(go.go) foo(go.txt) foo(plain.empty) foo(plain.go) foo(plain.txt)",
		language: "",
		filename: "file_without_extension",
	}, {
		name:     "No language and .go file extension => .go matcher",
		want:     "foo(go.empty) foo(go.go) foo(go.txt)",
		language: "",
		filename: "a/b/c/file.go",
	}, {
		name:     "Language Go and no file extension => .go matcher",
		want:     "foo(go.empty) foo(go.go) foo(go.txt)",
		language: "go",
		filename: "",
	}, {
		name:     "Language .go and .txt file extension => .go matcher",
		want:     "foo(go.empty) foo(go.go) foo(go.txt)",
		language: "go",
		filename: "file.txt",
	}}
	t.Run("group", func(t *testing.T) {
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				got := test(tc.language, tc.filename)
				if d := cmp.Diff(tc.want, got); d != "" {
					t.Errorf("mismatch (-want +got):\n%s", d)
				}
			})
		}
	})
}

// Tests that structural search correctly infers the Go matcher from the .go
// file extension.
func TestInferredMatcher(t *testing.T) {
	// If we are not on CI skip the test.
	if os.Getenv("CI") == "" {
		t.Skip("Not on CI, skipping comby-dependent test")
	}

	input := map[string]string{
		"main.go": `
/* This foo(ignore string) {} is in a Go comment should not match */
func foo(real string) {}
`,
	}

	pattern := "foo(:[args])"
	want := "foo(real string)"

	zipData, err := storetest.CreateZip(input)
	if err != nil {
		t.Fatal(err)
	}
	zPath, cleanup, err := storetest.TempZipFileOnDisk(zipData)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	zFile, _ := storetest.MockZipFile(zipData)
	if err != nil {
		t.Fatal(err)
	}

	p := &protocol.PatternInfo{
		Pattern: pattern,
		Limit:   30,
	}
	ctx, cancel, sender := newLimitedStreamCollector(context.Background(), 1000000000)
	defer cancel()
	err = filteredStructuralSearch(ctx, zPath, zFile, p, "foo", sender)
	if err != nil {
		t.Fatal(err)
	}
	got := sender.collected[0].LineMatches[0].Preview
	if err != nil {
		t.Fatal(err)
	}

	if got != want {
		t.Fatalf("got file matches %v, want %v", got, want)
	}
}

func TestRecordMetrics(t *testing.T) {
	cases := []struct {
		name            string
		language        []string
		includePatterns []string
		want            string
	}{
		{
			name:            "Empty values",
			language:        nil,
			includePatterns: []string{},
			want:            ".generic",
		},
		{
			name:            "Include patterns no extension",
			language:        nil,
			includePatterns: []string{"foo", "bar.go"},
			want:            ".generic",
		},
		{
			name:            "Include patterns first extension",
			language:        nil,
			includePatterns: []string{"foo.c", "bar.go"},
			want:            ".c",
		},
		{
			name:            "Non-empty language",
			language:        []string{"xml"},
			includePatterns: []string{"foo.c", "bar.go"},
			want:            ".xml",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var extensionHint string
			if len(tt.includePatterns) > 0 {
				filename := tt.includePatterns[0]
				extensionHint = filepath.Ext(filename)
			}
			got := toMatcher(tt.language, extensionHint)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

// Tests that includePatterns works. includePatterns serve a similar role in
// structural search compared to regex search, but is interpreted _differently_.
// includePatterns cannot be a regex expression (as in traditional search), but
// instead (currently) expects a list of patterns that represent a set of file
// paths to search.
func TestIncludePatterns(t *testing.T) {
	// If we are not on CI skip the test.
	if os.Getenv("CI") == "" {
		t.Skip("Not on CI, skipping comby-dependent test")
	}

	input := map[string]string{
		"/a/b/c":         "",
		"/a/b/c/foo.go":  "",
		"c/foo.go":       "",
		"bar.go":         "",
		"/x/y/z/bar.go":  "",
		"/a/b/c/nope.go": "",
		"nope.go":        "",
	}

	want := []string{
		"/a/b/c/foo.go",
		"/x/y/z/bar.go",
		"bar.go",
	}

	includePatterns := []string{"a/b/c/foo.go", "bar.go"}

	zipData, err := storetest.CreateZip(input)
	if err != nil {
		t.Fatal(err)
	}
	zf, cleanup, err := storetest.TempZipFileOnDisk(zipData)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	p := &protocol.PatternInfo{
		Pattern:         "",
		IncludePatterns: includePatterns,
	}
	ctx, cancel, sender := newLimitedStreamCollector(context.Background(), 1000000000)
	defer cancel()
	err = structuralSearch(ctx, zf, Subset(p.IncludePatterns), "", p.Pattern, p.CombyRule, p.Languages, "foo", sender)
	if err != nil {
		t.Fatal(err)
	}
	fileMatches := sender.collected

	got := make([]string, len(fileMatches))
	for i, fm := range fileMatches {
		got[i] = fm.Path
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got file matches %v, want %v", got, want)
	}
}

func TestRule(t *testing.T) {
	// If we are not on CI skip the test.
	if os.Getenv("CI") == "" {
		t.Skip("Not on CI, skipping comby-dependent test")
	}

	input := map[string]string{
		"file.go": "func foo(success) {} func bar(fail) {}",
	}

	zipData, err := storetest.CreateZip(input)
	if err != nil {
		t.Fatal(err)
	}
	zf, cleanup, err := storetest.TempZipFileOnDisk(zipData)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	p := &protocol.PatternInfo{
		Pattern:         "func :[[fn]](:[args])",
		IncludePatterns: []string{".go"},
		CombyRule:       `where :[args] == "success"`,
	}

	ctx, cancel, sender := newLimitedStreamCollector(context.Background(), 1000000000)
	defer cancel()
	err = structuralSearch(ctx, zf, Subset(p.IncludePatterns), "", p.Pattern, p.CombyRule, p.Languages, "repo", sender)
	if err != nil {
		t.Fatal(err)
	}
	got := sender.collected

	want := []protocol.FileMatch{
		{
			Path:     "file.go",
			LimitHit: false,
			LineMatches: []protocol.LineMatch{
				{
					LineNumber:       0,
					OffsetAndLengths: [][2]int{{0, 17}},
					Preview:          "func foo(success)",
				},
			},
			MatchCount: 1,
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got file matches %v, want %v", got, want)
	}
}

func TestStructuralLimits(t *testing.T) {
	// If we are not on CI skip the test.
	if os.Getenv("CI") == "" {
		t.Skip("Not on CI, skipping comby-dependent test")
	}

	input := map[string]string{
		"test1.go": `
func foo() {
    fmt.Println("foo")
}

func bar() {
    fmt.Println("bar")
}
`,
		"test2.go": `
func foo() {
    fmt.Println("foo")
}

func bar() {
    fmt.Println("bar")
}
`,
	}

	zipData, err := storetest.CreateZip(input)
	require.NoError(t, err)

	zf, cleanup, err := storetest.TempZipFileOnDisk(zipData)
	require.NoError(t, err)
	defer cleanup()

	count := func(matches []protocol.FileMatch) int {
		c := 0
		for _, match := range matches {
			c += match.MatchCount
		}
		return c
	}

	test := func(limit, wantCount int, p *protocol.PatternInfo) func(t *testing.T) {
		return func(t *testing.T) {
			ctx, cancel, sender := newLimitedStreamCollector(context.Background(), limit)
			defer cancel()
			err := structuralSearch(ctx, zf, Subset(p.IncludePatterns), "", p.Pattern, p.CombyRule, p.Languages, "repo_foo", sender)
			require.NoError(t, err)

			require.Equal(t, wantCount, count(sender.collected))
		}
	}

	t.Run("unlimited", test(10000, 4, &protocol.PatternInfo{Pattern: "{:[body]}"}))
	t.Run("exact limit", test(4, 4, &protocol.PatternInfo{Pattern: "{:[body]}"}))
	t.Run("limited", test(2, 2, &protocol.PatternInfo{Pattern: "{:[body]}"}))
	t.Run("many", test(12, 8, &protocol.PatternInfo{Pattern: "(:[_])"}))
}

func TestHighlightMultipleLines(t *testing.T) {
	cases := []struct {
		Name  string
		Match *comby.Match
		Want  []protocol.LineMatch
	}{
		{
			Name: "Single line",
			Match: &comby.Match{
				Range: comby.Range{
					Start: comby.Location{
						Line:   1,
						Column: 1,
					},
					End: comby.Location{
						Line:   1,
						Column: 2,
					},
				},
				Matched: "this is a single line match",
			},
			Want: []protocol.LineMatch{
				{
					LineNumber: 0,
					OffsetAndLengths: [][2]int{
						{
							0,
							1,
						},
					},
					Preview: "this is a single line match",
				},
			},
		},
		{
			Name: "Three lines",
			Match: &comby.Match{
				Range: comby.Range{
					Start: comby.Location{
						Line:   1,
						Column: 1,
					},
					End: comby.Location{
						Line:   3,
						Column: 5,
					},
				},
				Matched: "this is a match across\nthree\nlines",
			},
			Want: []protocol.LineMatch{
				{
					LineNumber: 0,
					OffsetAndLengths: [][2]int{
						{
							0,
							22,
						},
					},
					Preview: "this is a match across",
				},
				{
					LineNumber: 1,
					OffsetAndLengths: [][2]int{
						{
							0,
							5,
						},
					},
					Preview: "three",
				},
				{
					LineNumber: 2,
					OffsetAndLengths: [][2]int{
						{
							0,
							4, // don't include trailing newline
						},
					},
					Preview: "lines",
				},
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.Name, func(t *testing.T) {
			got := highlightMultipleLines(tt.Match)
			if !reflect.DeepEqual(got, tt.Want) {
				jsonGot, _ := json.Marshal(got)
				jsonWant, _ := json.Marshal(tt.Want)
				t.Errorf("got: %s, want: %s", jsonGot, jsonWant)
			}
		})
	}
}

func TestMatchCountForMultilineMatches(t *testing.T) {
	// If we are not on CI skip the test.
	if os.Getenv("CI") == "" {
		t.Skip("Not on CI, skipping comby-dependent test")
	}

	input := map[string]string{
		"main.go": `
func foo() {
    fmt.Println("foo")
}

func bar() {
    fmt.Println("bar")
}
`,
	}

	wantMatchCount := 2

	p := &protocol.PatternInfo{Pattern: "{:[body]}"}

	zipData, err := storetest.CreateZip(input)
	if err != nil {
		t.Fatal(err)
	}
	zf, cleanup, err := storetest.TempZipFileOnDisk(zipData)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	t.Run("Strutural search match count", func(t *testing.T) {
		ctx, cancel, sender := newLimitedStreamCollector(context.Background(), 1000000000)
		defer cancel()
		err := structuralSearch(ctx, zf, Subset(p.IncludePatterns), "", p.Pattern, p.CombyRule, p.Languages, "repo_foo", sender)
		if err != nil {
			t.Fatal(err)
		}
		matches := sender.collected
		var gotMatchCount int
		for _, fileMatches := range matches {
			gotMatchCount += fileMatches.MatchCount
		}
		if gotMatchCount != wantMatchCount {
			t.Fatalf("got match count %d, want %d", gotMatchCount, wantMatchCount)
		}
	})
}

func TestBuildQuery(t *testing.T) {
	pattern := ":[x~*]"
	want := "error parsing regexp: missing argument to repetition operator: `*`"
	t.Run("build query", func(t *testing.T) {
		_, err := buildQuery(&search.TextPatternInfo{Pattern: pattern}, nil, nil, false)
		if diff := cmp.Diff(err.Error(), want); diff != "" {
			t.Error(diff)
		}
	})
}
