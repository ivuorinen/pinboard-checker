// main_test.go
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExpandURL_Bitly(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"LongURL": "https://expanded.example.com"}`)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer server.Close()

	originalUnshortenBitly := unshortenBitly
	defer func() { unshortenBitly = originalUnshortenBitly }()

	unshortenBitly = func(_ string) (string, error) {
		return "https://expanded.example.com", nil
	}

	got, err := expandURL("bit.ly/test", false)
	if err != nil || got != "https://expanded.example.com" {
		t.Errorf("expected expanded URL, got %v, err: %v", got, err)
	}
}

func TestFixParenthesesSuffix(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.RequestURI, "fixed") {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	urlWithParen := server.URL + "/fixed)"
	fixed, err := fixParenthesesSuffix(urlWithParen, true)
	if err != nil || fixed == urlWithParen {
		t.Errorf("expected fixed url, got %v, err: %v", fixed, err)
	}
}

func TestUrlRedirects(t *testing.T) {
	t.Parallel()
	target := "https://example.org/final"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	redirected, err := urlRedirects(server.URL, false)
	if err != nil || redirected != target {
		t.Errorf("expected redirect to %s, got %v, err: %v", target, redirected, err)
	}
}

func TestUpdateBookmark_Error(t *testing.T) {
	t.Parallel()
	invalidToken := "invalid"
	bookmark := Bookmark{URL: "http://example.com", Title: "t", Notes: "n", Tags: "x y"}
	err := updateBookmark(invalidToken, bookmark, "http://example.com", "", "")
	if err == nil {
		t.Error("expected error when updating with invalid token")
	}
}

func TestDeleteBookmark_Error(t *testing.T) {
	t.Parallel()
	err := deleteBookmark("invalid", "http://nonexistent.com")
	if err == nil {
		t.Error("expected error on delete with invalid token")
	}
}

func TestGetPageTitle_ValidHTML(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		htmlContent := `<html><head><title>Test Page Title</title></head><body></body></html>`
		if _, err := w.Write([]byte(htmlContent)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer server.Close()

	title, err := getPageTitle(server.URL)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if title != "Test Page Title" {
		t.Errorf("expected 'Test Page Title', got %q", title)
	}
}

func TestGetPageTitle_NoTitle(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`<html><head></head><body></body></html>`)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer server.Close()

	title, err := getPageTitle(server.URL)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if title != "" {
		t.Errorf("expected empty title, got %q", title)
	}
}

func TestGetPageTitle_EmptyTitle(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		htmlContent := `<html><head><title></title></head><body></body></html>`
		if _, err := w.Write([]byte(htmlContent)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer server.Close()

	title, err := getPageTitle(server.URL)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if title != "" {
		t.Errorf("expected empty title, got %q", title)
	}
}

func TestGetPageTitle_WithWhitespace(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		htmlContent := `<html><head><title>  Trimmed Title  </title></head><body></body></html>`
		if _, err := w.Write([]byte(htmlContent)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer server.Close()

	title, err := getPageTitle(server.URL)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if title != "Trimmed Title" {
		t.Errorf("expected 'Trimmed Title', got %q", title)
	}
}

func TestGetPageTitle_404Error(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	title, err := getPageTitle(server.URL)
	if err == nil {
		t.Error("expected error for 404 status")
	}
	if title != "" {
		t.Errorf("expected empty title on error, got %q", title)
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies shared global pinboardAPIEndpoint
func TestGetSuggestedTags_PopularAndRecommended(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		jsonContent := `[{"popular":["go","golang","programming"],"recommended":["code","tutorial"]}]`
		if _, err := w.Write([]byte(jsonContent)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer server.Close()

	// Mock the API endpoint
	originalEndpoint := pinboardAPIEndpoint
	defer func() { pinboardAPIEndpoint = originalEndpoint }()
	pinboardAPIEndpoint = server.URL

	tags, err := getSuggestedTags("test-token", "http://example.com")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// Should have go,golang,programming,code,tutorial,.autoTagged
	if !strings.Contains(tags, ".autoTagged") {
		t.Error("expected .autoTagged tag to be included")
	}
	if !strings.Contains(tags, "go") {
		t.Error("expected 'go' tag from popular")
	}
	if !strings.Contains(tags, "tutorial") {
		t.Error("expected 'tutorial' tag from recommended")
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies shared global pinboardAPIEndpoint
func TestGetSuggestedTags_EmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`[]`)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := pinboardAPIEndpoint
	defer func() { pinboardAPIEndpoint = originalEndpoint }()
	pinboardAPIEndpoint = server.URL

	tags, err := getSuggestedTags("test-token", "http://example.com")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// Should only have .autoTagged when no suggestions
	if tags != ".autoTagged" {
		t.Errorf("expected only .autoTagged, got %q", tags)
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies shared global pinboardAPIEndpoint
func TestGetSuggestedTags_MaxTenTags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		jsonContent := `[{"popular":["a","b","c","d","e","f","g","h","i","j","k","l"],"recommended":[]}]`
		if _, err := w.Write([]byte(jsonContent)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := pinboardAPIEndpoint
	defer func() { pinboardAPIEndpoint = originalEndpoint }()
	pinboardAPIEndpoint = server.URL

	tags, err := getSuggestedTags("test-token", "http://example.com")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// Should have max 10 tags plus .autoTagged
	tagList := strings.Split(tags, ",")
	if len(tagList) != 11 { // 10 + .autoTagged
		t.Errorf("expected 11 tags (10 + .autoTagged), got %d: %v", len(tagList), tagList)
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies shared global pinboardAPIEndpoint
func TestGetSuggestedTags_404Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	originalEndpoint := pinboardAPIEndpoint
	defer func() { pinboardAPIEndpoint = originalEndpoint }()
	pinboardAPIEndpoint = server.URL

	tags, err := getSuggestedTags("test-token", "http://example.com")
	if err == nil {
		t.Error("expected error for 404 status")
	}
	if tags != "" {
		t.Errorf("expected empty tags on error, got %q", tags)
	}
}
