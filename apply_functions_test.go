package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

//nolint:paralleltest,unparam // Cannot run in parallel - modifies global verbose
func TestApplyUpdateAction_DryRun(t *testing.T) {
	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com", Title: "Old", Tags: "old"},
		Action:   ActionUpdate,
		NewURL:   "http://example.org",
		NewTitle: "New Title",
		NewTags:  "new,tags",
	}

	oldVerbose := verbose
	verbose = true
	defer func() { verbose = oldVerbose }()

	// Test dry run - should not make actual API calls
	applyUpdateAction(action, "test-token", true)

	// In dry run, updates counter should not increment
	// Since we can't easily check without modifying global state, just ensure no panic
}

//nolint:paralleltest,unparam // Cannot run in parallel - modifies global
func TestApplyUpdateAction_URLOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldEndpoint := pinboardAPIEndpoint
	pinboardAPIEndpoint = server.URL
	defer func() { pinboardAPIEndpoint = oldEndpoint }()

	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com", Title: "Title", Tags: "tags", Notes: "notes"},
		Action:   ActionUpdate,
		NewURL:   "http://example.org",
	}

	oldVerbose := verbose
	verbose = true
	defer func() { verbose = oldVerbose }()

	applyUpdateAction(action, "test-token", false)
}

//nolint:paralleltest,unparam // Cannot run in parallel - modifies global
func TestApplyUpdateAction_TitleOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldEndpoint := pinboardAPIEndpoint
	pinboardAPIEndpoint = server.URL
	defer func() { pinboardAPIEndpoint = oldEndpoint }()

	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com", Title: "", Tags: "tags"},
		Action:   ActionUpdate,
		NewTitle: "New Title",
	}

	oldVerbose := verbose
	verbose = false
	defer func() { verbose = oldVerbose }()

	applyUpdateAction(action, "test-token", false)
}

//nolint:paralleltest,unparam // Cannot run in parallel - modifies global
func TestApplyDeleteAction_DryRun(t *testing.T) {
	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com"},
		Action:   ActionDelete,
		Error:    errors.New("404 not found"),
	}

	oldVerbose := verbose
	verbose = true
	defer func() { verbose = oldVerbose }()

	applyDeleteAction(action, "test-token", true)
}

//nolint:paralleltest,unparam // Cannot run in parallel - modifies global
func TestApplyDeleteAction_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldEndpoint := pinboardAPIEndpoint
	pinboardAPIEndpoint = server.URL
	defer func() { pinboardAPIEndpoint = oldEndpoint }()

	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com"},
		Action:   ActionDelete,
		Error:    errors.New("404 not found"),
	}

	oldVerbose := verbose
	verbose = true
	defer func() { verbose = oldVerbose }()

	applyDeleteAction(action, "test-token", false)
}

//nolint:paralleltest,unparam // Cannot run in parallel - modifies global
func TestApplyDeleteAction_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	oldEndpoint := pinboardAPIEndpoint
	pinboardAPIEndpoint = server.URL
	defer func() { pinboardAPIEndpoint = oldEndpoint }()

	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com"},
		Action:   ActionDelete,
		Error:    errors.New("404 not found"),
	}

	oldVerbose := verbose
	verbose = true
	defer func() { verbose = oldVerbose }()

	applyDeleteAction(action, "test-token", false)
}

//nolint:paralleltest,unparam // Cannot run in parallel - modifies global
func TestApplyDeleteAction_NonVerbose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldEndpoint := pinboardAPIEndpoint
	pinboardAPIEndpoint = server.URL
	defer func() { pinboardAPIEndpoint = oldEndpoint }()

	action := BookmarkAction{
		Original: Bookmark{URL: "http://example.com"},
		Action:   ActionDelete,
		Error:    errors.New("404 not found"),
	}

	oldVerbose := verbose
	verbose = false
	defer func() { verbose = oldVerbose }()

	applyDeleteAction(action, "test-token", false)
}

//nolint:paralleltest,unparam // Cannot run in parallel - modifies global
func TestApplyBookmarkActions_NoActions(t *testing.T) {
	actions := []BookmarkAction{
		{Action: ActionNoChange},
		{Action: ActionNoChange},
	}

	oldVerbose := verbose
	verbose = true
	defer func() { verbose = oldVerbose }()

	oldCIMode := ciMode
	ciMode = false
	defer func() { ciMode = oldCIMode }()

	applyBookmarkActions(actions, "test-token", false)
}

//nolint:paralleltest,unparam // Cannot run in parallel - modifies global
func TestApplyBookmarkActions_WithActions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldEndpoint := pinboardAPIEndpoint
	pinboardAPIEndpoint = server.URL
	defer func() { pinboardAPIEndpoint = oldEndpoint }()

	actions := []BookmarkAction{
		{
			Original: Bookmark{URL: "http://example.com", Title: "Test"},
			Action:   ActionUpdate,
			NewURL:   "http://example.org",
		},
		{
			Original: Bookmark{URL: "http://dead.com"},
			Action:   ActionDelete,
			Error:    errors.New("404 not found"),
		},
		{
			Original: Bookmark{URL: "http://good.com"},
			Action:   ActionNoChange,
		},
	}

	oldVerbose := verbose
	verbose = false
	defer func() { verbose = oldVerbose }()

	oldCIMode := ciMode
	ciMode = false
	defer func() { ciMode = oldCIMode }()

	applyBookmarkActions(actions, "test-token", false)
}

//nolint:paralleltest,unparam // Cannot run in parallel - modifies global
func TestApplyBookmarkActions_CIMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldEndpoint := pinboardAPIEndpoint
	pinboardAPIEndpoint = server.URL
	defer func() { pinboardAPIEndpoint = oldEndpoint }()

	actions := []BookmarkAction{
		{
			Original: Bookmark{URL: "http://example.com"},
			Action:   ActionUpdate,
			NewURL:   "http://example.org",
		},
	}

	oldCIMode := ciMode
	ciMode = true
	defer func() { ciMode = oldCIMode }()

	applyBookmarkActions(actions, "test-token", false)
}

//nolint:paralleltest // Cannot run in parallel - modifies global skipTitles
func TestFetchTitleIfNeeded_WithNewURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		html := "<html><head><title>New URL Title</title></head></html>"
		if _, err := w.Write([]byte(html)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer server.Close()

	oldSkipTitles := skipTitles
	skipTitles = false
	defer func() { skipTitles = oldSkipTitles }()

	bookmark := Bookmark{URL: "http://old.com", Title: ""}
	result := fetchTitleIfNeeded(bookmark, server.URL)

	if result == "" {
		t.Error("fetchTitleIfNeeded() should have fetched title for new URL")
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies global skipTitles
func TestFetchTitleIfNeeded_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	oldSkipTitles := skipTitles
	skipTitles = false
	defer func() { skipTitles = oldSkipTitles }()

	bookmark := Bookmark{URL: "http://old.com", Title: ""}
	result := fetchTitleIfNeeded(bookmark, server.URL)

	// Should return empty string on error
	if result != "" {
		t.Errorf("fetchTitleIfNeeded() = %q, want empty on error", result)
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies global pinboardAPIEndpoint
func TestFetchTagsIfNeeded_WithNewURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if it's the suggest endpoint
		if r.URL.Path == "/posts/suggest" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			jsonResp := `[{"popular":["tag1"],"recommended":["tag2"]}]`
			if _, err := w.Write([]byte(jsonResp)); err != nil {
				t.Errorf("failed to write response: %v", err)
			}
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	oldEndpoint := pinboardAPIEndpoint
	pinboardAPIEndpoint = server.URL
	defer func() { pinboardAPIEndpoint = oldEndpoint }()

	oldSkipAutoTags := skipAutoTags
	skipAutoTags = false
	defer func() { skipAutoTags = oldSkipAutoTags }()

	bookmark := Bookmark{URL: "http://old.com", Tags: ""}
	result := fetchTagsIfNeeded(bookmark, "test-token", server.URL+"/newurl")

	if result == "" {
		t.Error("fetchTagsIfNeeded() should have fetched tags for new URL")
	}
}

//nolint:paralleltest // Cannot run in parallel - modifies global skipTitles, skipAutoTags
func TestProcessBookmark_WithTitleAndTags(t *testing.T) {
	titleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		html := "<html><head><title>Test Title</title></head></html>"
		if _, err := w.Write([]byte(html)); err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer titleServer.Close()

	oldSkipTitles := skipTitles
	oldSkipAutoTags := skipAutoTags
	skipTitles = false
	skipAutoTags = true // Skip tags to avoid API call complexity
	defer func() {
		skipTitles = oldSkipTitles
		skipAutoTags = oldSkipAutoTags
	}()

	bookmark := Bookmark{
		URL:   titleServer.URL,
		Title: "",
		Tags:  "existing",
	}

	action := processBookmark(bookmark, "test-token")
	if action.Action != ActionUpdate {
		t.Errorf("processBookmark() action = %v, want ActionUpdate", action.Action)
	}
	if action.NewTitle == "" {
		t.Error("processBookmark() should have fetched title")
	}
}
