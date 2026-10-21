package stream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// A small text file goes into the message; a large one (a 900 KB page) is
// saved and referenced by path, as a binary file is: inlined, it would swell
// every prompt past what one command-line argument can carry.
func TestResolveFilesInlinesOnlySmallText(t *testing.T) {
	big := "<html>" + strings.Repeat("<div>TaxFi</div>\n", 60<<10) + "</html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		name, body := "note.html", "<p>small</p>"
		if r.URL.Path == "/big" {
			name, body = "TaxFi Homepage Redesign.html", big
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	b := &Bot{client: NewClient(srv.URL, "t"), uploadsDir: t.TempDir()}

	got := b.resolveFiles(context.Background(), []string{"f_small", "f_big"},
		map[string]string{"f_small": srv.URL + "/small", "f_big": srv.URL + "/big"})
	if !strings.Contains(got, "--- File: note.html ---\n<p>small</p>") {
		t.Errorf("small file not inlined:\n%.300s", got)
	}
	if strings.Contains(got, "<div>TaxFi</div>") {
		t.Fatalf("the large file was inlined (%d bytes)", len(got))
	}
	m := regexp.MustCompile(`\[File 'TaxFi Homepage Redesign\.html': ([^\]]+)\]`).FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("large file not referenced by path:\n%s", got)
	}
	if saved, err := os.ReadFile(m[1]); err != nil || string(saved) != big {
		t.Errorf("saved file: %d bytes, err %v", len(saved), err)
	}
}
