package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEmitEvent_Success(t *testing.T) {
	var gotPath, gotAuth, gotContentType string
	var gotBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-token")
	payload := json.RawMessage(`{"key":"value"}`)
	err := c.EmitEvent(context.Background(), "conv-123", "status_update", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/v1/conversations/conv-123/events" {
		t.Errorf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("unexpected auth: %s", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("unexpected content-type: %s", gotContentType)
	}
	if gotBody["type"] != "status_update" {
		t.Errorf("unexpected event type: %v", gotBody["type"])
	}
}

func TestEmitEvent_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	err := c.EmitEvent(context.Background(), "conv-1", "test", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected status code in error, got: %v", err)
	}
}

func TestUploadFile_Success(t *testing.T) {
	var gotAuth string
	var gotContentType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")

		// Verify it's multipart
		if !strings.Contains(gotContentType, "multipart/form-data") {
			t.Errorf("expected multipart content type, got: %s", gotContentType)
		}

		// Verify path
		if r.URL.Path != "/v1/conversations/conv-1/files" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		// Read the multipart data to verify file
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("failed to read form file: %v", err)
			return
		}
		defer file.Close()

		if header.Filename != "test.txt" {
			t.Errorf("unexpected filename: %s", header.Filename)
		}

		data, _ := io.ReadAll(file)
		if string(data) != "file content" {
			t.Errorf("unexpected file data: %s", string(data))
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"file_id": "file-abc"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	fileID, err := c.UploadFile(context.Background(), "conv-1", "test.txt", "text/plain", []byte("file content"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fileID != "file-abc" {
		t.Errorf("expected file ID 'file-abc', got %q", fileID)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("unexpected auth: %s", gotAuth)
	}
}

func TestSendMessage_WithoutFileIDs(t *testing.T) {
	var gotBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/conversations/conv-1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	err := c.SendMessage(context.Background(), "conv-1", "hello world", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotBody["content"] != "hello world" {
		t.Errorf("unexpected content: %v", gotBody["content"])
	}
	if _, ok := gotBody["file_ids"]; ok {
		t.Error("file_ids should not be present when nil")
	}
}

func TestSendMessage_WithFileIDs(t *testing.T) {
	var gotBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	err := c.SendMessage(context.Background(), "conv-1", "msg", []string{"f1", "f2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fileIDs, ok := gotBody["file_ids"].([]interface{})
	if !ok {
		t.Fatal("expected file_ids array")
	}
	if len(fileIDs) != 2 {
		t.Errorf("expected 2 file_ids, got %d", len(fileIDs))
	}
}

func TestSendMessage_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("forbidden"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	err := c.SendMessage(context.Background(), "conv-1", "msg", nil)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected 403 in error, got: %v", err)
	}
}

func TestDownloadFile_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files/file-xyz" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("unexpected auth: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Disposition", `attachment; filename="report.txt"`)
		w.Write([]byte("file data here"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	df, err := c.DownloadFile(context.Background(), "file-xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(df.Data) != "file data here" {
		t.Errorf("unexpected data: %q", string(df.Data))
	}
	if df.Filename != "report.txt" {
		t.Errorf("expected filename 'report.txt', got %q", df.Filename)
	}
	if df.ContentType != "text/plain" {
		t.Errorf("expected content-type 'text/plain', got %q", df.ContentType)
	}
}

func TestDownloadFile_NoContentDisposition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("binary"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	df, err := c.DownloadFile(context.Background(), "file-id-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Without Content-Disposition, filename should fall back to the file ID
	if df.Filename != "file-id-123" {
		t.Errorf("expected filename to be file ID, got %q", df.Filename)
	}
}

func TestDownloadFile_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.DownloadFile(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 in error, got: %v", err)
	}
}

func TestStreamEvents_ParsesSSE(t *testing.T) {
	event1 := Event{Seq: 1, Type: "status", Payload: json.RawMessage(`{"state":"running"}`)}
	event2 := Event{Seq: 2, Type: "output", Payload: json.RawMessage(`{"text":"hello"}`)}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/conversations/conv-1/events/stream" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("after_seq") != "0" {
			t.Errorf("unexpected after_seq: %s", r.URL.Query().Get("after_seq"))
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("unexpected accept: %s", r.Header.Get("Accept"))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher")
		}

		// Write SSE events
		data1, _ := json.Marshal(event1)
		fmt.Fprintf(w, "data: %s\n\n", data1)
		flusher.Flush()

		data2, _ := json.Marshal(event2)
		fmt.Fprintf(w, "data: %s\n\n", data2)
		flusher.Flush()
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := c.StreamEvents(ctx, "conv-1", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var events []Event
	for e := range ch {
		events = append(events, e)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Seq != 1 || events[0].Type != "status" {
		t.Errorf("unexpected first event: %+v", events[0])
	}
	if events[1].Seq != 2 || events[1].Type != "output" {
		t.Errorf("unexpected second event: %+v", events[1])
	}
}

func TestStreamEvents_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("unauthorized"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.StreamEvents(context.Background(), "conv-1", 0)
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error, got: %v", err)
	}
}
