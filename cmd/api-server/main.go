package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"

	"Grimoire/internal/model/job"
)

func main() {
	// Initialize queue
	job.InitQueue()

	app := fiber.New()

	// Add middleware
	app.Use(logger.New())
	app.Use(cors.New())

	// Setup API routes
	SetupRoutes(app)

	// Setup graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		log.Println("Shutting down gracefully...")
		job.Shutdown()
		app.Shutdown()
	}()

	log.Println("API Server listening on http://localhost:8081")
	log.Fatal(app.Listen(":8081"))
}

// SetupRoutes configures all API routes
func SetupRoutes(app *fiber.App) {
	app.Post("/api/submit", handleSubmit)
	app.Get("/api/:id", handleGetJob)
	app.Get("/api/:id/pdf", handleGetJobPDF)
	app.Get("/api/jobs", handleGetAllJobs)
}

func handleSubmit(c *fiber.Ctx) error {
	decklist := c.FormValue("Decklist")
	if decklist == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Decklist is required",
		})
	}

	// Create and enqueue job
	jobInstance, err := job.CreateJob(decklist)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create job: " + err.Error(),
		})
	}

	return c.JSON(fiber.Map{
		"job_id": jobInstance.ID,
		"status": "queued",
	})
}

func handleGetJob(c *fiber.Ctx) error {
	jobID := c.Params("id")
	jobInstance, exists := job.GetJob(jobID)
	if !exists {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Job not found",
		})
	}

	status, err := jobInstance.GetStatus()
	response := fiber.Map{
		"job_id": jobID,
		"status": status,
	}

	if err != nil {
		response["error"] = err.Error()
	}

	return c.JSON(response)
}

func handleGetJobPDF(c *fiber.Ctx) error {
	jobID := c.Params("id")
	jobInstance, exists := job.GetJob(jobID)
	if !exists {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Job not found",
		})
	}

	status, err := jobInstance.GetStatus()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Job failed: " + err.Error(),
		})
	}

	if status != "complete" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Job not complete, current status: " + status,
		})
	}

	buf := jobInstance.GetPDF()
	if buf == nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "PDF not available",
		})
	}

	c.Set("Content-Type", "application/pdf")
	c.Set("Content-Disposition", "attachment; filename=decklist.pdf")
	c.Set("Content-Length", fmt.Sprintf("%d", buf.Len()))

	return c.Send(buf.Bytes())
}

func handleGetAllJobs(c *fiber.Ctx) error {
	allJobs := job.GetAllJobs()
	response := make(map[string]any, len(allJobs))

	for id, j := range allJobs {
		status, err := j.GetStatus()
		jobInfo := map[string]any{
			"status": status,
		}
		if err != nil {
			jobInfo["error"] = err.Error()
		}
		response[id] = jobInfo
	}
	return c.JSON(response)
}
