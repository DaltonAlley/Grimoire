package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/signintech/gopdf"
)

type Card struct {
	Quantity        int
	Set             string
	CollectorNumber string
	Layout          string `json:"layout"`
	ImageURIs       map[string]string
}

func submitDecklist(decklist string) ([]Card, error) {
	lines := strings.Split(decklist, "\n")

	// Regex: qty, name, set, collector (may contain letters/numbers/hyphens/slashes)
	re := regexp.MustCompile(`^(\d+)\s+(.+)\s+\(([^)]+)\)\s+([^\s]+)$`)

	var cards []Card
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		matches := re.FindStringSubmatch(line)
		if matches == nil {
			return nil, fmt.Errorf("Could not parse line: %s", line)
		}
		quantity, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("Could not parse quantity: %w", err)
		}
		var card Card
		card.Set = matches[3]
		card.CollectorNumber = matches[4]
		card.Quantity = quantity
		url := fmt.Sprintf("https://api.scryfall.com/cards/%s/%s", card.Set, card.CollectorNumber)
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("API error: status %d", resp.StatusCode)
		}

		if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
			fmt.Printf("JSON error: %v\n", err)
			return nil, err
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

		cards = append(cards, card)
	}
	return cards, nil
}

func generatePDF(cards []Card) (*bytes.Buffer, error) {
	pageW, pageH := 197.0, 269.0
	var buf bytes.Buffer
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: gopdf.Rect{W: pageW, H: pageH}})

	for _, card := range cards {
		for range make([]struct{}, card.Quantity) {
			for _, imageURI := range card.ImageURIs {
				pdf.AddPage()

				pdf.SetFillColor(0, 0, 0)
				pdf.Rectangle(0, 0, pageW, pageH, "F", 0, 0)

				resp, err := http.Get(imageURI)
				if err != nil {
					log.Print(err.Error())
					return nil, err
				}
				defer resp.Body.Close()

				imgHolder, err := gopdf.ImageHolderByReader(resp.Body)
				if err != nil {
					log.Print(err.Error())
					return nil, err
				}

				x, y := (pageW-180)/2, (pageH-252)/2
				pdf.ImageByHolder(imgHolder, x, y, &gopdf.Rect{W: 180, H: 252})
			}
		}
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
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		if err := index().Render(context.Background(), w); err != nil {
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
