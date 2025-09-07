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

type Card struct {
	Quantity        int
	Name            string
	Set             string
	CollectorNumber string
	Layout          string `json:"layout"`
	ImageURIs       map[string]string
}

func parseCard(line string, client *http.Client) (Card, error) {
	// Regex: handle standard format and basic lands (e.g., "1 Plains")
	re := regexp.MustCompile(`^(\d+)\s+(.+?)(?:\s+\(([^)]+)\)\s+([^\s]+))?$`)

	line = strings.TrimSpace(line)
	if line == "" {
		return Card{}, fmt.Errorf("empty line")
	}

	matches := re.FindStringSubmatch(line)
	if matches == nil {
		return Card{}, fmt.Errorf("could not parse line: %s", line)
	}

	quantity, err := strconv.Atoi(matches[1])
	if err != nil {
		return Card{}, fmt.Errorf("invalid quantity: %w", err)
	}

	card := Card{
		Quantity:        quantity,
		Name:            matches[2],
		Set:             matches[3], // May be empty for basic lands
		CollectorNumber: matches[4], // May be empty for basic lands
	}

	url := fmt.Sprintf("https://api.scryfall.com/cards/%s/%s", card.Set, card.CollectorNumber)
	resp, err := client.Get(url)
	if err != nil {
		return Card{}, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Card{}, fmt.Errorf("API error: status %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return Card{}, fmt.Errorf("JSON decode failed: %w", err)
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

func submitDecklist(decklist string) ([]Card, error) {
	lines := strings.Split(decklist, "\n")
	log.Printf("Parsing %d lines", len(lines))

	// Filter out empty lines
	var nonEmptyLines []string
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonEmptyLines = append(nonEmptyLines, line)
		}
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var wg sync.WaitGroup
	cardChan := make(chan Card)
	errChan := make(chan error, len(nonEmptyLines))
	var cardsCompleted int
	var mu sync.Mutex // For thread-safe logging and counter

	// Launch goroutines
	for _, line := range nonEmptyLines {
		wg.Add(1)
		go func(line string) {
			defer wg.Done()

			card, err := parseCard(line, client)
			if err != nil {
				errChan <- fmt.Errorf("failed to parse %q: %w", line, err)
				return
			}

			mu.Lock()
			cardsCompleted++
			log.Printf("Parsed card: %s (%d / %d cards completed)", card.Name, cardsCompleted, len(nonEmptyLines))
			mu.Unlock()

			cardChan <- card
		}(line)
	}

	// Close channels when all goroutines are done
	go func() {
		wg.Wait()
		close(cardChan)
		close(errChan)
	}()

	// Collect results
	var cards []Card
	for card := range cardChan {
		cards = append(cards, card)
	}

	// Collect errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
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

	// Concurrently fetch images
	var wg sync.WaitGroup
	wg.Add(len(allURIs))
	for i, uri := range allURIs {
		go func(i int, uri string) {
			defer wg.Done()
			resp, err := http.Get(uri)
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				errs[i] = err
				return
			}
			imageData[i] = body
		}(i, uri)
	}
	wg.Wait()

	// Check for any fetch errors (abort on first error to match original behavior)
	for _, err := range errs {
		if err != nil {
			log.Print(err.Error())
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
		}

		buf, err := generatePDF(cards)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
