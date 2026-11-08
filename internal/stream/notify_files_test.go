package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// A background session's result goes to each channel with its files
// attached; a file that fails to upload is named in the message instead.
func TestSendNotificationFilesAttachesUploads(t *testing.T) {
	var mu sync.Mutex
	var uploads []string
	var sent []OutMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/files"):
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "broken.csv") {
				http.Error(w, "too large", http.StatusRequestEntityTooLarge)
				return
			}
			uploads = append(uploads, r.URL.Path)
			fmt.Fprintf(w, `{"file_id":"f_%d"}`, len(uploads))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			var m OutMessage
			json.NewDecoder(r.Body).Decode(&m)
			sent = append(sent, m)
			fmt.Fprint(w, `{"message_id":"m_1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b := &Bot{client: NewClient(srv.URL, "t"), listeners: map[string]context.CancelFunc{"c_one": func() {}}}
	files := []agent.OutputFile{
		{Name: "watchlist-2026-11-09.md", ContentType: "text/markdown", Data: []byte("# report")},
		{Name: "broken.csv", ContentType: "text/csv", Data: []byte("a,b")},
	}
	if err := b.SendNotificationFiles(context.Background(), "今日自选股报告", files); err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 1 || uploads[0] != "/v2/channels/c_one/files" {
		t.Errorf("uploads: %v", uploads)
	}
	if len(sent) != 1 {
		t.Fatalf("messages: %+v", sent)
	}
	m := sent[0]
	if len(m.FileIDs) != 1 || m.FileIDs[0] != "f_1" {
		t.Errorf("file ids: %v", m.FileIDs)
	}
	if !strings.HasPrefix(m.Content, "今日自选股报告") || !strings.Contains(m.Content, "upload failed): broken.csv") {
		t.Errorf("content: %q", m.Content)
	}
}
