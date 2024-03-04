package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	pinboardAPIEndpoint = "https://api.pinboard.in/v1/"
)

type Bookmark struct {
	Title string `json:"description"`
	URL   string `json:"href"`
}

func main() {
	apiToken := flag.String("token", "", "Pinboard API token")
	dryRun := flag.Bool("dry-run", false, "Dry run (print actions without modifying bookmarks)")
	verbose := flag.Bool("verbose", false, "Verbose mode")
	flag.Parse()

	if *apiToken == "" {
		fmt.Println("Please provide a Pinboard API token.")
		flag.Usage()
		os.Exit(1)
	}

	bookmarks, err := getBookmarks(*apiToken, *verbose)
	if err != nil {
		fmt.Println("Error fetching bookmarks:", err)
		return
	}

	processBookmarks(bookmarks, verbose, dryRun, apiToken)
}

func processBookmarks(bookmarks []Bookmark, verbose *bool, dryRun *bool, apiToken *string) {
	for _, bookmark := range bookmarks {
		if *verbose {
			fmt.Printf("Processing bookmark: %s\n", bookmark.URL)
		}
		newURL, err := expandAndCheckURL(bookmark.URL, *verbose, *dryRun)

		if err == nil && newURL != "" && newURL != bookmark.URL {
			if *verbose {
				fmt.Printf("Bookmark has been expanded, using the new '%s'\n", newURL)
			}

			if !*dryRun {
				err := updateBookmark(*apiToken, bookmark.URL, newURL)
				if err != nil {
					fmt.Printf("Error updating bookmark '%s': %v\n", bookmark.URL, err)
					continue
				}
			}
			fmt.Printf("Updated bookmark: %s -> %s\n", bookmark.URL, newURL)
		}

		if err != nil {
			fmt.Printf("Error processing bookmark '%s': %v\n", bookmark.URL, err)
			if !*dryRun {
				err := deleteBookmark(*apiToken, bookmark.URL)
				if err != nil {
					fmt.Printf("Error deleting bookmark '%s': %v\n", bookmark.URL, err)
					continue
				}
				fmt.Printf("Deleted bookmark: %s\n", bookmark.URL)
			} else {
				fmt.Printf("Dry run: Bookmark '%s' would have been deleted\n", bookmark.URL)
			}
		}
	}
}

func getBookmarks(apiToken string, verbose bool) ([]Bookmark, error) {
	resp, err := http.Get(fmt.Sprintf("%sposts/all?auth_token=%s&format=json", pinboardAPIEndpoint, apiToken))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var bookmarks []Bookmark
	if err := json.NewDecoder(resp.Body).Decode(&bookmarks); err != nil {
		return nil, err
	}

	if verbose {
		fmt.Printf("Got %d bookmarks from pinboard.in\n", len(bookmarks))
	}

	return bookmarks, nil
}

func expandAndCheckURL(url string, verbose, dryRun bool) (string, error) {
	// Check for expansion (is the URL a short URL?)
	expandedURL, err := expandURL(url)
	if err != nil {
		return "", err
	}
	expandedResp, err := http.Head(expandedURL)
	if err != nil {
		return "", err
	}
	defer expandedResp.Body.Close()

	if expandedResp.StatusCode >= 400 {
		return "", fmt.Errorf("expanded URL returns non-success status code: %d", expandedResp.StatusCode)
	}

	// Check if the url has ")" at the end, fix if needed
	fixedUrl, err := fixParenthesesSuffix(expandedURL, verbose)
	if err != nil {
		return "", err
	}
	fixedResp, err := http.Head(fixedUrl)
	if err != nil {
		return "", err
	}
	defer fixedResp.Body.Close()

	if fixedResp.StatusCode >= 400 {
		return "", fmt.Errorf("fixed URL returns non-success status code: %d", fixedResp.StatusCode)
	}

	// Check if the URL redirects to another URL, use the final URL if so
	redirectedUrl, err := urlRedirects(fixedUrl, verbose)
	if err != nil {
		return "", err
	}
	redirectResp, err := http.Head(redirectedUrl)
	if err != nil {
		return "", err
	}
	defer redirectResp.Body.Close()

	if redirectResp.StatusCode >= 400 {
		return "", fmt.Errorf("redirected URL returns non-success status code: %d", fixedResp.StatusCode)
	}

	return redirectedUrl, nil
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
		return "", err
	}
	defer resp.Body.Close()

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
	resp, err := http.Head(urlString)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

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

func expandURL(shortURL string) (string, error) {
	var s = shortURL
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")

	fmt.Printf("-> Prefix removed '%s'\n", s)

	if strings.HasPrefix(s, "bit.ly/") {
		return unshortenBitly(shortURL)
	} else if strings.HasPrefix(s, "tinyurl.com/") {
		return unshortenGeneric(shortURL, "http://tinyurl.com/api-create.php?=url=")
	} else if strings.HasPrefix(s, "is.gd/") {
		return unshortenGeneric(shortURL, "https://is.gd/forward.php?format=json&shorturl=")
	}

	return shortURL, nil
}

func unshortenBitly(shortURL string) (string, error) {
	resp, err := http.Post(
		"https://api-ssl.bitly.com/v4/expand",
		"application/json",
		strings.NewReader(fmt.Sprintf(`{"short_url": "%s"}`, shortURL)),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var bitlyResp struct{ LongURL string }
	if err := json.NewDecoder(resp.Body).Decode(&bitlyResp); err != nil {
		return "", err
	}

	return bitlyResp.LongURL, nil
}

func unshortenGeneric(shortURL, apiURL string) (string, error) {
	resp, err := http.Get(apiURL + shortURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	longURL, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(longURL), nil
}

func updateBookmark(apiToken, oldURL, newURL string) error {
	if newURL == "" {
		return deleteBookmark(apiToken, oldURL)
	}

	resp, err := http.Post(
		pinboardAPIEndpoint+"posts/add",
		"application/x-www-form-urlencoded",
		strings.NewReader(fmt.Sprintf("url=%s&replace=yes&old=%s&auth_token=%s", newURL, oldURL, apiToken)),
	)
	if err != nil {
		fmt.Printf("(!) Error updating bookmark: %v\n", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

func deleteBookmark(apiToken, url string) error {
	resp, err := http.Post(
		pinboardAPIEndpoint+"posts/delete",
		"application/x-www-form-urlencoded",
		strings.NewReader(fmt.Sprintf("url=%s&auth_token=%s", url, apiToken)),
	)
	if err != nil {
		fmt.Printf("(!) Error deleting bookmark: %v\n", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}
