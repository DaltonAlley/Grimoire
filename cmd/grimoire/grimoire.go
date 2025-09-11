package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	templates "Grimoire/internal/templates"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/signintech/gopdf"
)

// Simple rate limiter to ensure no more than 10 requests per second
var lastRequestTime time.Time
var rateLimiterMutex sync.Mutex

func rateLimitWait() {
	rateLimiterMutex.Lock()
	defer rateLimiterMutex.Unlock()

	now := time.Now()
	elapsed := now.Sub(lastRequestTime)

	// Ensure at least 100ms between requests (10 requests per second)
	if elapsed < 100*time.Millisecond {
		time.Sleep(100*time.Millisecond - elapsed)
	}

	lastRequestTime = time.Now()
}

type Card struct {
	Quantity        int
	Name            string
	Set             string
	CollectorNumber string
	Layout          string `json:"layout"`
	ImageURIs       map[string]string
}

func parseCard(line string, client *http.Client) (Card, error) {
	// More robust regex that handles various card name formats and edge cases
	// This regex is more permissive with whitespace and handles special characters better
	re := regexp.MustCompile(`^(\d+)\s+(.+?)\s+\(([^)]+)\)\s+([^\s\r\n]+)$`)

	line = strings.TrimSpace(line)
	if line == "" {
		return Card{}, fmt.Errorf("empty line")
	}

	matches := re.FindStringSubmatch(line)
	if matches == nil {
		// Try a more permissive regex as fallback
		fallbackRe := regexp.MustCompile(`^(\d+)\s+(.+?)\s+\(([^)]+)\)\s+(.+)$`)
		matches = fallbackRe.FindStringSubmatch(line)
		if matches == nil {
			return Card{}, fmt.Errorf("could not parse line: %q", line)
		}
		// Clean up the collector number from any trailing whitespace
		matches[4] = strings.TrimSpace(matches[4])
	}

	quantity, err := strconv.Atoi(matches[1])
	if err != nil {
		return Card{}, fmt.Errorf("invalid quantity: %w", err)
	}

	card := Card{
		Quantity:        quantity,
		Name:            strings.TrimSpace(matches[2]),
		Set:             matches[3],
		CollectorNumber: matches[4],
	}

	// Retry logic for rate limiting
	maxRetries := 3
	baseDelay := 100 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Rate limit API requests
		rateLimitWait()

		url := fmt.Sprintf("https://api.scryfall.com/cards/%s/%s", card.Set, card.CollectorNumber)
		resp, err := client.Get(url)
		if err != nil {
			if attempt == maxRetries-1 {
				return Card{}, fmt.Errorf("HTTP request failed after %d attempts: %w", maxRetries, err)
			}
			time.Sleep(baseDelay * time.Duration(1<<attempt)) // Exponential backoff
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			if attempt == maxRetries-1 {
				return Card{}, fmt.Errorf("API rate limited after %d attempts", maxRetries)
			}
			// Wait much longer for rate limit (5s, 10s)
			delay := 5 * time.Second * time.Duration(1<<attempt)
			log.Printf("Rate limited, waiting %v before retry %d/%d", delay, attempt+2, maxRetries+1)
			time.Sleep(delay)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return Card{}, fmt.Errorf("API error: status %d", resp.StatusCode)
		}

		if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
			resp.Body.Close()
			return Card{}, fmt.Errorf("JSON decode failed: %w", err)
		}
		resp.Body.Close()

		// Success - break out of retry loop
		break
	}

	if card.Layout == "transform" || card.Layout == "modal_dfc" {
		card.ImageURIs = map[string]string{
			"front": fmt.Sprintf("https://api.scryfall.com/cards/%s/%s?format=image&version=png", card.Set, card.CollectorNumber),
			"back":  fmt.Sprintf("https://api.scryfall.com/cards/%s/%s?format=image&version=png&face=back", card.Set, card.CollectorNumber),
		}
	} else {
		card.ImageURIs = map[string]string{
			"front": fmt.Sprintf("https://api.scryfall.com/cards/%s/%s?format=image&version=png", card.Set, card.CollectorNumber),
		}
	}

	return card, nil
}

// fetchImageWithRetry fetches an image with retry logic
func fetchImageWithRetry(uri string, maxRetries int) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			log.Printf("Retrying image fetch for %s (attempt %d/%d) after %v delay", uri, attempt+1, maxRetries+1, delay)
			time.Sleep(delay)
		}

		// Rate limit image requests
		rateLimitWait()

		resp, err := http.Get(uri)
		if err != nil {
			lastErr = err
			log.Printf("Image fetch attempt %d failed for %s: %v", attempt+1, uri, err)
			continue
		}

		// Check for HTTP error status
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP error: status %d", resp.StatusCode)
			log.Printf("Image fetch attempt %d failed for %s: %v", attempt+1, uri, lastErr)

			// Special handling for rate limit (429) - wait longer
			if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
				delay := 5 * time.Second * time.Duration(1<<attempt)
				log.Printf("Rate limited on image fetch, waiting %v before retry", delay)
				time.Sleep(delay)
			}
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			log.Printf("Image read attempt %d failed for %s: %v", attempt+1, uri, err)
			continue
		}

		// Success
		if attempt > 0 {
			log.Printf("Image fetch succeeded for %s on attempt %d", uri, attempt+1)
		}
		return body, nil
	}

	return nil, fmt.Errorf("failed to fetch image after %d attempts: %w", maxRetries+1, lastErr)
}

func submitDecklist(decklist string) ([]Card, error) {
	// Normalize line endings to handle both Unix (\n) and Windows (\r\n)
	decklist = strings.ReplaceAll(decklist, "\r\n", "\n")
	decklist = strings.ReplaceAll(decklist, "\r", "\n")

	lines := strings.Split(decklist, "\n")
	log.Printf("Parsing %d lines", len(lines))

	var nonEmptyLines []string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			nonEmptyLines = append(nonEmptyLines, line)
		} else {
			log.Printf("Filtering out empty line %d: %q", i+1, line)
		}
	}

	log.Printf("After filtering: %d non-empty lines", len(nonEmptyLines))

	if len(nonEmptyLines) == 0 {
		return nil, nil // No lines to parse, return empty result
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second, // Increased timeout for retries
	}

	// Rate limiting: limit concurrent requests to 1 since we have global rate limiter
	maxConcurrent := 1
	semaphore := make(chan struct{}, maxConcurrent)

	var wg sync.WaitGroup
	// Use a single channel to send both card results and errors
	resultsChan := make(chan struct {
		card Card
		err  error
	}, len(nonEmptyLines)) // Buffered channel to avoid blocking goroutines

	var cardsCompleted int
	var mu sync.Mutex // For thread-safe logging and counter

	// Launch goroutines with rate limiting
	for _, line := range nonEmptyLines {
		wg.Add(1)
		go func(line string) {
			defer wg.Done()

			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			card, err := parseCard(line, client)
			if err != nil {
				log.Printf("Failed to parse line: %q, error: %v", line, err)
				resultsChan <- struct {
					card Card
					err  error
				}{card: Card{}, err: fmt.Errorf("failed to parse %q: %w", line, err)}
				return
			}

			mu.Lock()
			cardsCompleted++
			log.Printf("Parsed card: %s (%d / %d cards completed)", card.Name, cardsCompleted, len(nonEmptyLines))
			mu.Unlock()

			resultsChan <- struct {
				card Card
				err  error
			}{card: card, err: nil}
		}(line)
	}

	// Close the results channel when all goroutines are done
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect results and errors from the single channel
	var cards []Card
	var errors []error
	for res := range resultsChan {
		if res.err != nil {
			errors = append(errors, res.err)
		} else {
			cards = append(cards, res.card)
		}
	}

	if len(errors) > 0 {
		return nil, fmt.Errorf("encountered %d errors: %v", len(errors), errors)
	}

	return cards, nil
}

func generatePDF(cards []Card) (*bytes.Buffer, error) {
	pageW, pageH := 197.0, 269.0
	var buf bytes.Buffer
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: gopdf.Rect{W: pageW, H: pageH}})

	// Collect all image URIs in the order they will be added to the PDF
	var allURIs []string
	var collectorNumbers []string // To preserve logging per card
	for _, card := range cards {
		for q := 0; q < card.Quantity; q++ {
			for _, imageURI := range card.ImageURIs {
				allURIs = append(allURIs, imageURI)
				collectorNumbers = append(collectorNumbers, card.CollectorNumber)
			}
		}
	}

	if len(allURIs) == 0 {
		_, err := pdf.WriteTo(&buf)
		if err != nil {
			log.Print(err.Error())
			return nil, err
		}
		return &buf, nil
	}

	// Prepare slices for results and errors
	imageData := make([][]byte, len(allURIs))
	errs := make([]error, len(allURIs))

	// Concurrently fetch images with retry logic
	var wg sync.WaitGroup
	wg.Add(len(allURIs))
	for i, uri := range allURIs {
		go func(i int, uri string) {
			defer wg.Done()

			// Fetch image with retry (2 additional attempts = 3 total attempts)
			body, err := fetchImageWithRetry(uri, 2)
			if err != nil {
				errs[i] = err
				return
			}
			imageData[i] = body
		}(i, uri)
	}
	wg.Wait()

	// Check for any fetch errors (abort on first error to match original behavior)
	for i, err := range errs {
		if err != nil {
			log.Printf("Failed to fetch image %d (%s): %v", i+1, allURIs[i], err)
			return nil, err
		}
	}

	// Sequentially add pages to PDF
	for i := range allURIs {
		log.Printf("Adding page for %s", collectorNumbers[i])

		pdf.AddPage()

		pdf.SetFillColor(0, 0, 0)
		pdf.Rectangle(0, 0, pageW, pageH, "F", 0, 0)

		imgHolder, err := gopdf.ImageHolderByReader(bytes.NewReader(imageData[i]))
		if err != nil {
			log.Print(err.Error())
			return nil, err
		}

		x, y := (pageW-180)/2, (pageH-252)/2
		pdf.ImageByHolder(imgHolder, x, y, &gopdf.Rect{W: 180, H: 252})
		log.Printf("Finished page for %s", collectorNumbers[i])
	}

	_, err := pdf.WriteTo(&buf)
	if err != nil {
		log.Print(err.Error())
		return nil, err
	}
	return &buf, nil
}

func main() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	// Serve static files from the web/static directory (relative to project root)
	staticPath := filepath.Join("..", "..", "web", "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir(staticPath))))

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		if err := templates.Index().Render(context.Background(), w); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	r.Post("/submit", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		cards, err := submitDecklist(r.FormValue("Decklist"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		buf, err := generatePDF(cards)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", "attachment; filename=decklist.pdf")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))

		if _, err := w.Write(buf.Bytes()); err != nil {
			http.Error(w, "Failed to write PDF to response", http.StatusInternalServerError)
			return
		}
	})

	fmt.Println("Listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", r))
}
