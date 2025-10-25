package job

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-queue/queue"
	"github.com/golang-queue/queue/core"
	"github.com/golang-queue/queue/job"
	"github.com/google/uuid"
	"github.com/signintech/gopdf"
)

// Global queue and job storage
var q *queue.Queue
var jobs sync.Map

// GrimoireJob represents a decklist processing job
type GrimoireJob struct {
	ID        string
	Status    string        // "queued", "parse", "fetch", "generate", "complete", "error"
	PDF       *bytes.Buffer // Store the generated PDF
	Error     error
	CreatedAt time.Time
	mu        sync.RWMutex
}

// DecklistTask is the enqueued task payload
type DecklistTask struct {
	JobID    string `json:"job_id"`
	Decklist string `json:"decklist"`
}

func (dt *DecklistTask) Bytes() []byte {
	b, err := json.Marshal(dt)
	if err != nil {
		log.Printf("Failed to marshal task: %v", err)
		return nil
	}
	return b
}

// Card represents a Magic: The Gathering card
type Card struct {
	Quantity        int
	Name            string
	Set             string
	CollectorNumber string
	Layout          string `json:"layout"`
	ImageURIs       map[string]string
}

// Rate limiter variables
var lastRequestTime time.Time
var rateLimiterMutex sync.Mutex

func rateLimitWait() {
	rateLimiterMutex.Lock()
	defer rateLimiterMutex.Unlock()

	now := time.Now()
	elapsed := now.Sub(lastRequestTime)

	if elapsed < 100*time.Millisecond {
		time.Sleep(100*time.Millisecond - elapsed)
	}

	lastRequestTime = time.Now()
}

// InitQueue initializes the queue with efficient settings
func InitQueue() {
	workers := runtime.NumCPU() // Dynamic worker count
	workers = max(workers, 2)
	q = queue.NewPool(
		int64(workers),
		queue.WithFn(processWrapper), // Wrapper for message handling + cleanup
		queue.WithQueueSize(100),     // Buffer size to prevent blocking
	)

	// Periodic cleanup for old jobs
	go cleanupJobs()
}

// Shutdown gracefully shuts down the queue
func Shutdown() {
	if q != nil {
		log.Println("Shutting down queue...")
		q.Release()
	}
}

// CreateJob creates a job and enqueues it with per-task timeout
func CreateJob(decklist string) (*GrimoireJob, error) {
	jobInstance := NewGrimoireJob()
	jobs.Store(jobInstance.ID, jobInstance)

	// Enqueue task with 2-minute per-task timeout
	task := &DecklistTask{JobID: jobInstance.ID, Decklist: decklist}
	opts := []job.AllowOption{
		{Timeout: job.Time(2 * time.Minute)},
	}
	if err := q.Queue(task, opts...); err != nil {
		jobs.Delete(jobInstance.ID) // Rollback on enqueue failure
		return nil, err
	}

	return jobInstance, nil
}

// GetJob retrieves a job by ID
func GetJob(id string) (*GrimoireJob, bool) {
	j, exists := jobs.Load(id)
	if !exists {
		return nil, false
	}
	return j.(*GrimoireJob), true
}

// GetAllJobs returns all jobs
func GetAllJobs() map[string]*GrimoireJob {
	result := make(map[string]*GrimoireJob)
	jobs.Range(func(key, value any) bool {
		result[key.(string)] = value.(*GrimoireJob)
		return true
	})
	return result
}

// cleanupJobs removes old jobs (fallback)
func cleanupJobs() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		var toDelete []string
		jobs.Range(func(key, value any) bool {
			j := value.(*GrimoireJob)
			if time.Since(j.CreatedAt) > 1*time.Hour {
				toDelete = append(toDelete, key.(string))
			}
			return true
		})
		for _, id := range toDelete {
			jobs.Delete(id)
			log.Printf("Cleaned up job %s (expired)", id)
		}
	}
}

// processWrapper wraps the handler for cleanup
func processWrapper(ctx context.Context, m core.TaskMessage) error {
	// Unwrap DecklistTask and process
	var dt DecklistTask
	if err := json.Unmarshal(m.Payload(), &dt); err != nil {
		return err
	}

	// Run the actual handler
	err := ProcessDecklistHandler(ctx, m)

	// Immediate cleanup on completion or error - Disabled for now to prevent loss of jobs
	// - Enable when storing completed jobs in a database
	// -
	// if _, exists := GetJob(dt.JobID); exists {
	// 	jobs.Delete(dt.JobID)
	// 	log.Printf("Cleaned up job %s after completion/error", dt.JobID)
	// }

	return err
}

func NewGrimoireJob() *GrimoireJob {
	return &GrimoireJob{
		ID:        uuid.New().String(),
		Status:    "queued",
		CreatedAt: time.Now(),
	}
}

func (j *GrimoireJob) setStatus(status string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = status
}

func (j *GrimoireJob) setError(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Error = err
	j.Status = "error"
}

func (j *GrimoireJob) setPDF(pdf *bytes.Buffer) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.PDF = pdf
}

func (j *GrimoireJob) GetStatus() (string, error) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.Status, j.Error
}

func (j *GrimoireJob) GetPDF() *bytes.Buffer {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.PDF
}

// ProcessDecklistHandler is the queue task handler
func ProcessDecklistHandler(ctx context.Context, m core.TaskMessage) error {
	var dt DecklistTask
	if err := json.Unmarshal(m.Payload(), &dt); err != nil {
		return fmt.Errorf("failed to unmarshal task: %w", err)
	}

	// Get job from storage
	job, exists := GetJob(dt.JobID)
	if !exists {
		return fmt.Errorf("job %s not found", dt.JobID)
	}

	job.setStatus("parse")

	// Use decklist from task payload
	decklist := strings.ReplaceAll(dt.Decklist, "\r\n", "\n")
	decklist = strings.ReplaceAll(decklist, "\r", "\n")

	lines := strings.Split(decklist, "\n")
	log.Printf("Job %s: Parsing %d lines", dt.JobID, len(lines))

	var nonEmptyLines []string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			nonEmptyLines = append(nonEmptyLines, line)
		} else {
			log.Printf("Job %s: Filtering out empty line %d: %q", dt.JobID, i+1, line)
		}
	}

	log.Printf("Job %s: After filtering: %d non-empty lines", dt.JobID, len(nonEmptyLines))

	if len(nonEmptyLines) == 0 {
		job.setStatus("complete")
		return nil
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	maxConcurrent := 1
	semaphore := make(chan struct{}, maxConcurrent)

	var wg sync.WaitGroup
	resultsChan := make(chan struct {
		card Card
		err  error
	}, len(nonEmptyLines))

	var cardsCompleted int
	var mu sync.Mutex

	for _, line := range nonEmptyLines {
		wg.Add(1)
		go func(line string) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// Manual retry for ParseCard
			var card Card
			var err error
			for attempt := 0; attempt < 3; attempt++ {
				card, err = ParseCard(line, client)
				if err == nil {
					break
				}
				log.Printf("Job %s: Parse attempt %d failed for %q: %v", dt.JobID, attempt+1, line, err)
				if attempt < 2 {
					time.Sleep(time.Second * time.Duration(attempt+1)) // Exponential backoff
				}
			}

			if err != nil {
				log.Printf("Job %s: Failed to parse line: %q, error: %v", dt.JobID, line, err)
				resultsChan <- struct {
					card Card
					err  error
				}{card: Card{}, err: fmt.Errorf("failed to parse %q: %w", line, err)}
				return
			}

			log.Printf("Job %s: Successfully parsed card: %s (Set: %s, Collector: %s)", dt.JobID, card.Name, card.Set, card.CollectorNumber)

			mu.Lock()
			cardsCompleted++
			log.Printf("Job %s: Parsed card: %s (%d / %d cards completed)", dt.JobID, card.Name, cardsCompleted, len(nonEmptyLines))
			mu.Unlock()

			resultsChan <- struct {
				card Card
				err  error
			}{card: card, err: nil}
		}(line)
	}

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

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
		err := fmt.Errorf("encountered %d errors: %v", len(errors), errors)
		job.setError(err)
		return err
	}

	job.setStatus("fetch")

	job.setStatus("generate")
	pdfBuffer, err := GeneratePDF(cards)
	if err != nil {
		job.setError(fmt.Errorf("PDF generation failed: %w", err))
		return err
	}

	job.setPDF(pdfBuffer)
	job.setStatus("complete")

	return nil
}

func ParseCard(line string, client *http.Client) (Card, error) {
	re := regexp.MustCompile(`^(\d+)\s+(.+?)\s+\(([^)]+)\)\s+([^\s\r\n]+)$`)

	line = strings.TrimSpace(line)
	if line == "" {
		return Card{}, fmt.Errorf("empty line")
	}

	matches := re.FindStringSubmatch(line)
	if matches == nil {
		fallbackRe := regexp.MustCompile(`^(\d+)\s+(.+?)\s+\(([^)]+)\)\s+(.+)$`)
		matches = fallbackRe.FindStringSubmatch(line)
		if matches == nil {
			return Card{}, fmt.Errorf("could not parse line: %q", line)
		}
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

	maxRetries := 3
	baseDelay := 100 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		rateLimitWait()

		url := fmt.Sprintf("https://api.scryfall.com/cards/%s/%s", card.Set, card.CollectorNumber)
		resp, err := client.Get(url)
		if err != nil {
			if attempt == maxRetries-1 {
				return Card{}, fmt.Errorf("HTTP request failed after %d attempts: %w", maxRetries, err)
			}
			time.Sleep(baseDelay * time.Duration(1<<attempt))
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			if attempt == maxRetries-1 {
				return Card{}, fmt.Errorf("API rate limited after %d attempts", maxRetries)
			}
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

func FetchImageWithRetry(uri string, maxRetries int) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			log.Printf("Retrying image fetch for %s (attempt %d/%d) after %v delay", uri, attempt+1, maxRetries+1, delay)
			time.Sleep(delay)
		}

		rateLimitWait()

		resp, err := http.Get(uri)
		if err != nil {
			lastErr = err
			log.Printf("Image fetch attempt %d failed for %s: %v", attempt+1, uri, err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP error: status %d", resp.StatusCode)
			log.Printf("Image fetch attempt %d failed for %s: %v", attempt+1, uri, lastErr)

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

// convertTo8Bit converts a 16-bit image to 8-bit for gopdf compatibility
func convertTo8Bit(imageData []byte) ([]byte, error) {
	// Decode the image
	img, _, err := image.Decode(bytes.NewReader(imageData))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	// Create a new 8-bit RGBA image
	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)

	// Draw the original image onto the new RGBA image
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)

	// Always encode as JPEG for better gopdf compatibility (avoids PNG parser issues)
	var buf bytes.Buffer
	err = jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: 95})
	if err != nil {
		return nil, fmt.Errorf("failed to encode image: %w", err)
	}

	return buf.Bytes(), nil
}

func GeneratePDF(cards []Card) (*bytes.Buffer, error) {
	pageW, pageH := 197.0, 269.0
	var buf bytes.Buffer
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: gopdf.Rect{W: pageW, H: pageH}})

	var allURIs []string
	var cardNames []string
	for _, card := range cards {
		for q := 0; q < card.Quantity; q++ {
			for _, imageURI := range card.ImageURIs {
				allURIs = append(allURIs, imageURI)
				cardNames = append(cardNames, card.Name)
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

	imageData := make([][]byte, len(allURIs))
	errs := make([]error, len(allURIs))

	var wg sync.WaitGroup
	wg.Add(len(allURIs))
	for i, uri := range allURIs {
		go func(i int, uri string) {
			defer wg.Done()

			log.Printf("Fetching image for %s: %s", cardNames[i], uri)
			body, err := FetchImageWithRetry(uri, 2)
			if err != nil {
				log.Printf("Failed to fetch image for %s: %v", cardNames[i], err)
				errs[i] = err
				return
			}
			log.Printf("Successfully fetched image for %s (%d bytes)", cardNames[i], len(body))
			imageData[i] = body
		}(i, uri)
	}
	wg.Wait()

	var failedImages []int
	for i, err := range errs {
		if err != nil {
			log.Printf("Failed to fetch image %d (%s) for card %s: %v", i+1, allURIs[i], cardNames[i], err)
			failedImages = append(failedImages, i)
		}
	}

	if len(failedImages) > 0 {
		log.Printf("Warning: Failed to fetch %d out of %d images. Continuing with available images.", len(failedImages), len(allURIs))
	}

	for i := range allURIs {
		// Skip failed images
		if errs[i] != nil {
			log.Printf("Skipping page for %s due to failed image fetch", cardNames[i])
			continue
		}

		log.Printf("Adding page for %s", cardNames[i])

		pdf.AddPage()

		pdf.SetFillColor(0, 0, 0)
		pdf.Rectangle(0, 0, pageW, pageH, "F", 0, 0)

		// Convert image to 8-bit if necessary
		convertedImageData, err := convertTo8Bit(imageData[i])
		if err != nil {
			log.Printf("Failed to convert image for %s: %v", cardNames[i], err)
			continue // Skip this image instead of failing the entire PDF
		}

		imgHolder, err := gopdf.ImageHolderByReader(bytes.NewReader(convertedImageData))
		if err != nil {
			log.Printf("Failed to create image holder for %s: %v", cardNames[i], err)
			continue // Skip this image instead of failing the entire PDF
		}

		x, y := (pageW-180)/2, (pageH-252)/2
		pdf.ImageByHolder(imgHolder, x, y, &gopdf.Rect{W: 180, H: 252})
		log.Printf("Finished page for %s", cardNames[i])
	}

	_, err := pdf.WriteTo(&buf)
	if err != nil {
		log.Print(err.Error())
		return nil, err
	}
	return &buf, nil
}
