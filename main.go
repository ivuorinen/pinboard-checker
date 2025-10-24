package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/net/html"
)

const (
	ErrClosingBodyMsg = "Error closing response body: %v\n"
	tokenName         = "PINBOARD_API_TOKEN" //nolint:gosec // Not a credential, just the env var name
)

var pinboardAPIEndpoint = "https://api.pinboard.in/v1"

type Bookmark struct {
	Title string `json:"description"`
	URL   string `json:"href"`
	Tags  string `json:"tags,omitempty"`
	Notes string `json:"extended,omitempty"`
}

var (
	verbose        bool
	ciMode         bool
	skipTitles     bool
	skipAutoTags   bool
	rateLimiter    = time.NewTicker(3 * time.Second)
	rateLimiterMux sync.Mutex
)

func main() {
	// Load .env file if it exists (error is OK if file doesn't exist)
	_ = godotenv.Load() //nolint:errcheck // .env file is optional

	apiTokenEnv, _ := os.LookupEnv(tokenName)
	apiToken := flag.String("token", apiTokenEnv, "Pinboard API token")
	dryRun := flag.Bool("dry-run", false, "Dry run mode")
	flag.BoolVar(&verbose, "verbose", false, "Verbose mode")
	flag.BoolVar(&ciMode, "ci", false, "CI mode (no progress bar or verbose output)")
	flag.BoolVar(&skipTitles, "skip-titles", false, "Skip fetching titles for bookmarks without them")
	flag.BoolVar(&skipAutoTags, "skip-auto-tags", false, "Skip auto-tagging bookmarks without tags")
	flag.Parse()

	if *apiToken == "" {
		fmt.Println("Error: API token is required")
		os.Exit(1)
	}

	bookmarks, err := getBookmarks(*apiToken)
	if err != nil {
		fmt.Println("Error fetching bookmarks:", err)

		return
	}

	processBookmarksParallel(bookmarks, *apiToken, *dryRun)
}

func processBookmark(b Bookmark, apiToken string, dryRun bool) {
	if verbose {
		fmt.Printf("Processing: %s\n", b.URL)
	}
	newURL, err := expandAndCheckURL(b.URL, verbose)

	newTitle := fetchTitleIfNeeded(b, newURL)
	newTags := fetchTagsIfNeeded(b, apiToken, newURL)

	if err != nil {
		handleBookmarkError(b, apiToken, dryRun)

		return
	}

	if shouldUpdateBookmark(b, newURL, newTitle, newTags) {
		performBookmarkUpdate(b, apiToken, newURL, newTitle, newTags, dryRun)
	}
}

func shouldUpdateBookmark(b Bookmark, newURL, newTitle, newTags string) bool {
	urlChanged := newURL != "" && newURL != b.URL
	titleAdded := newTitle != ""
	tagsAdded := newTags != ""

	return urlChanged || titleAdded || tagsAdded
}

func handleBookmarkError(b Bookmark, apiToken string, dryRun bool) {
	if dryRun {
		return
	}
	if deleteErr := deleteBookmark(apiToken, b.URL); deleteErr == nil && verbose {
		fmt.Printf("Deleted: %s\n", b.URL)
	}
}

func performBookmarkUpdate(b Bookmark, apiToken, newURL, newTitle, newTags string, dryRun bool) {
	if dryRun {
		return
	}

	urlToUpdate := newURL
	if urlToUpdate == "" {
		urlToUpdate = b.URL
	}

	if err := updateBookmark(apiToken, b, urlToUpdate, newTitle, newTags); err != nil {
		return
	}

	if !verbose {
		return
	}

	if newURL != "" && newURL != b.URL {
		fmt.Printf("Updated: %s -> %s\n", b.URL, newURL)
	}
	if newTitle != "" {
		fmt.Printf("Added title: %s\n", newTitle)
	}
	if newTags != "" {
		fmt.Printf("Added tags: %s\n", newTags)
	}
}

func fetchTitleIfNeeded(b Bookmark, newURL string) string {
	if skipTitles || strings.TrimSpace(b.Title) != "" {
		return ""
	}

	urlToFetch := b.URL
	if newURL != "" {
		urlToFetch = newURL
	}

	fetchedTitle, err := getPageTitle(urlToFetch)
	if err == nil && fetchedTitle != "" {
		if verbose {
			fmt.Printf("Fetched title: %s\n", fetchedTitle)
		}

		return fetchedTitle
	}

	return ""
}

func fetchTagsIfNeeded(b Bookmark, apiToken, newURL string) string {
	if skipAutoTags || strings.TrimSpace(b.Tags) != "" {
		return ""
	}

	urlToFetch := b.URL
	if newURL != "" {
		urlToFetch = newURL
	}

	fetchedTags, err := getSuggestedTags(apiToken, urlToFetch)
	if err == nil && fetchedTags != "" {
		if verbose {
			fmt.Printf("Fetched tags: %s\n", fetchedTags)
		}

		return fetchedTags
	}

	return ""
}

func processBookmarksParallel(bookmarks []Bookmark, apiToken string, dryRun bool) {
	bar := progressbar.Default(int64(len(bookmarks)))
	if ciMode {
		bar = nil
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 10) // limit concurrency

	for _, bookmark := range bookmarks {
		wg.Add(1)
		sem <- struct{}{}
		go func(b Bookmark) {
			defer wg.Done()
			defer func() { <-sem }()
			if bar != nil {
				_ = bar.Add(1) //nolint:errcheck // Progress bar errors are non-critical
			}
			processBookmark(b, apiToken, dryRun)
		}(bookmark)
	}
	wg.Wait()
}

func waitForRateLimit() {
	rateLimiterMux.Lock()
	defer rateLimiterMux.Unlock()
	<-rateLimiter.C
}

// getBookmarks retrieves all bookmarks from Pinboard API.
// Security Note: Pinboard API requires auth_token in query parameters (not headers).
// This is a limitation of Pinboard's API design. Tokens may appear in server logs.
// Always use HTTPS (enforced by Pinboard) and keep tokens secure.

func getBookmarks(apiToken string) ([]Bookmark, error) {
	waitForRateLimit()
	resp, err := http.Get(fmt.Sprintf("%s/posts/all?auth_token=%s&format=json",
		pinboardAPIEndpoint,
		url.QueryEscape(apiToken)))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch bookmarks from Pinboard API: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("Error closing response body: %v\n", err)
		}
	}()
	var bookmarks []Bookmark
	if err := json.NewDecoder(resp.Body).Decode(&bookmarks); err != nil {
		return nil, fmt.Errorf("failed to decode bookmarks JSON: %w", err)
	}

	return bookmarks, nil
}

// updateBookmark updates a bookmark with a new URL in Pinboard.
// Security Note: Pinboard API requires auth_token in query parameters (not headers).
// This is a limitation of Pinboard's API design. Tokens may appear in server logs.

func updateBookmark(apiToken string, b Bookmark, newURL, newTitle, newTags string) error {
	waitForRateLimit()

	// Use new title if provided, otherwise use existing title
	titleToUse := b.Title
	if newTitle != "" {
		titleToUse = newTitle
	}

	// Use new tags if provided, otherwise use existing tags
	tagsToUse := b.Tags
	if newTags != "" {
		tagsToUse = newTags
	}
	// Convert spaces to commas for Pinboard API
	tagsToUse = strings.ReplaceAll(tagsToUse, " ", ",")

	req := fmt.Sprintf(
		"%s/posts/add?url=%s&description=%s&extended=%s&tags=%s&replace=yes&auth_token=%s",
		pinboardAPIEndpoint,
		url.QueryEscape(newURL),
		url.QueryEscape(titleToUse),
		url.QueryEscape(b.Notes),
		url.QueryEscape(tagsToUse),
		url.QueryEscape(apiToken),
	)
	resp, err := http.Get(req)
	if err != nil {
		return fmt.Errorf("failed to update bookmark via Pinboard API: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("Error closing response body: %v\n", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update failed: %d", resp.StatusCode)
	}

	return nil
}

// deleteBookmark removes a bookmark from Pinboard.
// Security Note: Pinboard API requires auth_token in query parameters (not headers).
// This is a limitation of Pinboard's API design. Tokens may appear in server logs.

func deleteBookmark(apiToken, urlStr string) error {
	waitForRateLimit()
	resp, err := http.Get(fmt.Sprintf("%s/posts/delete?url=%s&auth_token=%s",
		pinboardAPIEndpoint,
		url.QueryEscape(urlStr),
		url.QueryEscape(apiToken)))
	if err != nil {
		return fmt.Errorf("failed to delete bookmark via Pinboard API: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("Error closing response body: %v\n", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("delete failed: %d", resp.StatusCode)
	}

	return nil
}

// getPageTitle fetches a URL and extracts the <title> tag content.
// Returns empty string if title cannot be extracted (no fallback).
func getPageTitle(urlString string) (string, error) {
	resp, err := http.Get(urlString)
	if err != nil {
		return "", fmt.Errorf("failed to fetch page for title extraction: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("Error closing response body: %v\n", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch page: status %d", resp.StatusCode)
	}

	doc, err := html.Parse(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to parse HTML: %w", err)
	}

	title := extractTitleFromHTML(doc)

	return strings.TrimSpace(title), nil
}

func extractTitleFromHTML(node *html.Node) string {
	if node.Type == html.ElementNode && node.Data == "title" {
		if node.FirstChild != nil {
			return node.FirstChild.Data
		}

		return ""
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if title := extractTitleFromHTML(child); title != "" {
			return title
		}
	}

	return ""
}

// getSuggestedTags fetches tag suggestions from Pinboard API for a given URL.
// Returns up to 10 suggested tags plus .autoTagged, comma-separated.
func getSuggestedTags(apiToken, urlStr string) (string, error) {
	waitForRateLimit()

	apiURL := fmt.Sprintf("%s/posts/suggest?url=%s&auth_token=%s",
		pinboardAPIEndpoint,
		url.QueryEscape(urlStr),
		url.QueryEscape(apiToken))

	resp, err := http.Get(apiURL)
	if err != nil {
		return "", fmt.Errorf("failed to fetch tag suggestions from Pinboard API: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("Error closing response body: %v\n", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get suggestions: status %d", resp.StatusCode)
	}

	var suggestions []struct {
		Popular     []string `json:"popular"`
		Recommended []string `json:"recommended"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&suggestions); err != nil {
		return "", fmt.Errorf("failed to decode tag suggestions JSON: %w", err)
	}

	tags := collectTagsFromSuggestions(suggestions)

	return strings.Join(tags, ","), nil
}

func collectTagsFromSuggestions(suggestions []struct {
	Popular     []string `json:"popular"`
	Recommended []string `json:"recommended"`
}) []string {
	tagMap := make(map[string]bool)
	var tags []string

	if len(suggestions) > 0 {
		for _, tag := range suggestions[0].Popular {
			if !tagMap[tag] && len(tags) < 10 {
				tagMap[tag] = true
				tags = append(tags, tag)
			}
		}
		for _, tag := range suggestions[0].Recommended {
			if !tagMap[tag] && len(tags) < 10 {
				tagMap[tag] = true
				tags = append(tags, tag)
			}
		}
	}

	// Always add .autoTagged tag
	tags = append(tags, ".autoTagged")

	return tags
}

func expandAndCheckURL(url string, verbose bool) (string, error) {
	// Check for expansion (is the URL a short URL?)
	expandedURL, err := expandURL(url, verbose)
	if err != nil {
		return "", err
	}
	if err := validateURLAccessibility(expandedURL, "expanded URL"); err != nil {
		return "", err
	}

	// Check if the url has ")" at the end, fix if needed
	fixedURL, err := fixParenthesesSuffix(expandedURL, verbose)
	if err != nil {
		return "", err
	}
	if err := validateURLAccessibility(fixedURL, "fixed URL"); err != nil {
		return "", err
	}

	// Check if the URL redirects to another URL, use the final URL if so
	redirectedURL, err := urlRedirects(fixedURL, verbose)
	if err != nil {
		return "", err
	}
	if err := validateURLAccessibility(redirectedURL, "redirected URL"); err != nil {
		return "", err
	}

	return redirectedURL, nil
}

func validateURLAccessibility(urlString, context string) error {
	resp, err := http.Head(urlString)
	if err != nil {
		return fmt.Errorf("failed to check %s: %w", context, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			fmt.Printf(ErrClosingBodyMsg, closeErr)
		}
	}()

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return fmt.Errorf("%s returns non-success status code: %d", context, resp.StatusCode)
	}

	return nil
}

func fixParenthesesSuffix(urlString string, verbose bool) (string, error) {
	// Check if URL ends with ")", if it doesn't return early
	if !strings.HasSuffix(urlString, ")") {
		return urlString, nil
	}

	// Retry check without ")"
	updatedURL := strings.TrimSuffix(urlString, ")")
	if verbose {
		fmt.Printf("URL '%s' ends with ')', retrying without it.\n", urlString)
	}
	resp, err := http.Head(updatedURL)
	if err != nil {
		return "", fmt.Errorf("failed to check URL without parenthesis: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			fmt.Printf(ErrClosingBodyMsg, closeErr)
		}
	}()

	// If the updated URL works, return it
	if resp.StatusCode == http.StatusOK {
		if verbose {
			fmt.Printf("URL updated: '%s' -> '%s'\n", urlString, updatedURL)
		}

		return updatedURL, nil
	}

	return urlString, nil
}

func urlRedirects(urlString string, verbose bool) (string, error) {
	// Create a client that doesn't follow redirects
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Head(urlString)
	if err != nil {
		return "", fmt.Errorf("failed to check URL for redirects: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			fmt.Printf(ErrClosingBodyMsg, closeErr)
		}
	}()

	// If redirected, update the URL
	if resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound {
		redirectURL := resp.Header.Get("Location")
		if redirectURL != "" {
			if verbose {
				fmt.Printf("Redirected URL updated: '%s' -> '%s'\n", urlString, redirectURL)
			}

			return redirectURL, nil
		}
	}

	return urlString, nil
}

//goland:noinspection HttpUrlsUsage
func expandURL(shortURL string, verbose bool) (string, error) {
	cleanedURL := shortURL
	cleanedURL = strings.TrimPrefix(cleanedURL, "https://")
	cleanedURL = strings.TrimPrefix(cleanedURL, "http://")
	cleanedURL = strings.TrimPrefix(cleanedURL, "www.")

	if verbose {
		fmt.Printf("-> Prefix removed '%s'\n", cleanedURL)
	}

	switch {
	case strings.HasPrefix(cleanedURL, "bit.ly/"):
		return unshortenBitly(shortURL)
	case strings.HasPrefix(cleanedURL, "tinyurl.com/"):
		return unshortenGeneric(shortURL, "http://tinyurl.com/api-create.php?=url=")
	case strings.HasPrefix(cleanedURL, "is.gd/"):
		return unshortenGeneric(shortURL, "https://is.gd/forward.php?format=json&shorturl=")
	default:
		return shortURL, nil
	}
}

// unshortenBitly is a variable that can be overridden for testing.
var unshortenBitly = unshortenBitlyImpl

func unshortenBitlyImpl(shortURL string) (string, error) {
	resp, err := http.Post(
		"https://api-ssl.bitly.com/v4/expand",
		"application/json",
		strings.NewReader(fmt.Sprintf(`{"short_url": "%s"}`, shortURL)),
	)
	if err != nil {
		return "", fmt.Errorf("failed to expand bit.ly URL: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			fmt.Printf(ErrClosingBodyMsg, closeErr)
		}
	}()

	var bitlyResp struct{ LongURL string }
	if err := json.NewDecoder(resp.Body).Decode(&bitlyResp); err != nil {
		return "", fmt.Errorf("failed to decode bit.ly response: %w", err)
	}

	return bitlyResp.LongURL, nil
}

func unshortenGeneric(shortURL, apiURL string) (string, error) {
	resp, err := http.Get(apiURL + shortURL)
	if err != nil {
		return "", fmt.Errorf("failed to expand short URL: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			fmt.Printf(ErrClosingBodyMsg, closeErr)
		}
	}()

	longURL, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read URL expansion response: %w", err)
	}

	return string(longURL), nil
}
