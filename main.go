package main

import (
	"bytes"
	"context"
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

type CardEntry struct {
	Quantity        string
	Set             string
	CollectorNumber string
	DoubleSided     bool
}

type Card struct {
	Quantity  int
	ImageURIs map[string]string
}

func parseDecklist(decklist string) []CardEntry {
	lines := strings.Split(decklist, "\n")

	// Regex: qty, name, set, collector (may contain letters/numbers/hyphens/slashes)
	re := regexp.MustCompile(`^(\d+)\s+(.+)\s+\(([^)]+)\)\s+([^\s]+)$`)

	var cards []CardEntry
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		matches := re.FindStringSubmatch(line)
		if matches == nil {
			fmt.Println("Could not parse line:", line)
			continue
		}
		doubleSided := strings.Contains(matches[2], "//")
		cards = append(cards, CardEntry{
			Quantity:        matches[1],
			Set:             matches[3],
			CollectorNumber: matches[4],
			DoubleSided:     doubleSided,
		})
	}
	return cards
}

func submitDecklist(cardEntries []CardEntry) ([]Card, error) {
	var cards []Card
	for _, cardEntry := range cardEntries {
		quantity, err := strconv.Atoi(cardEntry.Quantity)
		if err != nil {
			return nil, err
		}

		front := fmt.Sprintf("https://api.scryfall.com/cards/%s/%s?format=image&version=png", cardEntry.Set, cardEntry.CollectorNumber)
		if cardEntry.DoubleSided {
			back := fmt.Sprintf("https://api.scryfall.com/cards/%s/%s?format=image&version=png&face=back", cardEntry.Set, cardEntry.CollectorNumber)
			cards = append(cards, Card{
				Quantity: quantity,
				ImageURIs: map[string]string{
					"front": front,
					"back":  back,
				},
			})
		} else {
			cards = append(cards, Card{
				Quantity: quantity,
				ImageURIs: map[string]string{
					"front": front,
				},
			})
		}
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

		cardEntries := parseDecklist(r.FormValue("Decklist"))

		cards, err := submitDecklist(cardEntries)
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
